// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package relay is the cluster-side half of the supervisor's push tunnel.
//
// Why it exists. A worker in this cluster cannot reach the Mac that spawned it: the Mac's
// inbound surface drops every port except a couple that something else already allows, and
// no amount of cluster-side configuration changes that. The direction that does work is the
// Mac reaching the cluster, so the tunnel is inverted — the Mac long-polls this relay and the
// relay holds what arrives in between.
//
// ⚠️ THE RELAY DOES NOT VERIFY THE NOTIFICATION TOKEN, AND THAT IS DELIBERATE. The party that
// mints a credential is the party that verifies it: the supervisor mints a per-worker token
// and its own receiver compares it against the tokens it issued. A relay that also "validated"
// would have to hold a copy of every live token, which is a second authority over the same
// secret and a wider blast radius for nothing. The relay therefore requires only that a token
// be present — it is the routing key — and queues the body unread. The Mac drops anything whose
// token its registry does not know, exactly as it does today.
//
// ⚠️ AND THAT IS WHY THE PULL SIDE IS AUTHENTICATED INSTEAD. Pushes are unauthenticated by
// design, so anyone able to reach the pull endpoint could drain them. The tunnel secret is what
// keeps the queue private, and it is held by the Mac only.
package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bborbe/errors"
	"github.com/golang/glog"
)

const (
	// TokenHeader is the header the A2A SDK's push sender sets. Canonicalised, because Go
	// canonicalises on the server side and a differently-cased constant would silently miss.
	TokenHeader = "A2A-Notification-Token"

	// TunnelSecretHeader authenticates the Mac's pull. Never the token header: a push is a
	// worker's claim, a pull is the operator's channel, and one header for both would let a
	// worker drain the queue.
	//
	//nolint:gosec // G101: this is a header NAME, not a credential. The value it names is
	// supplied by the deployment and never appears in this package.
	TunnelSecretHeader = "X-A2A-Tunnel-Secret"

	// PushPath is what a worker posts to. It mirrors the receiver's own path so the worker's
	// registration is the only thing that changes, not its behaviour.
	PushPath = "/relay/push"

	// PullPath is what the Mac long-polls.
	PullPath = "/relay/pull"

	// MaxPerToken bounds one worker's queue, matching the receiver's own bound. A status
	// channel is a rolling window of recent facts, not a log: unbounded growth here is a slow
	// leak in a process that lives as long as the Deployment.
	MaxPerToken = 50

	// MaxBodyBytes caps a single push. The SDK's status events are small; a body larger than
	// this is not a status and is refused rather than buffered.
	MaxBodyBytes = 1 << 20

	// DefaultWait is the long-poll window. Short enough that a lost connection is noticed
	// quickly, long enough that an idle tunnel costs a handful of requests a minute.
	DefaultWait = 25 * time.Second

	// MaxWait bounds what a caller may ask for, so a client cannot pin a goroutine for ever.
	MaxWait = 60 * time.Second
)

// Item is one queued push: the token it was routed by, and the body the worker sent.
type Item struct {
	Token string          `json:"token"`
	Body  json.RawMessage `json:"body"`
}

// Store is the per-token queue the relay fills and the Mac drains.
//
// ⚠️ A SINGLE NOTIFY CHANNEL, NOT ONE PER TOKEN. The Mac pulls everything at once and
// dispatches by token itself, so a per-token signal would only add bookkeeping the consumer
// does not use.
type Store struct {
	mu       sync.Mutex
	byToken  map[string][]json.RawMessage
	total    int
	maxTotal int
	notify   chan struct{}
}

// NewStore builds a store bounded by the total number of queued items.
//
// ⚠️ THE TOTAL BOUND IS THE ONE THAT MATTERS. A per-token bound alone is not a bound at all:
// tokens are minted per spawn, so a stream of new tokens would grow the map without ever
// tripping any single token's cap.
func NewStore(maxTotal int) *Store {
	if maxTotal <= 0 {
		maxTotal = 500
	}
	return &Store{
		byToken:  make(map[string][]json.RawMessage),
		maxTotal: maxTotal,
		notify:   make(chan struct{}, 1),
	}
}

// Push queues one body under its token.
//
// A full store refuses rather than dropping the oldest: a dropped status is invisible, while a
// refused push reaches the worker's own log, where the failure can be seen.
func (s *Store) Push(token string, body json.RawMessage) error {
	if token == "" {
		return errors.New(context.Background(), "push requires a non-empty notification token")
	}
	s.mu.Lock()
	if s.total >= s.maxTotal {
		s.mu.Unlock()
		return errors.Errorf(context.Background(), "relay queue is full (%d items)", s.total)
	}
	queue := s.byToken[token]
	if len(queue) >= MaxPerToken {
		s.mu.Unlock()
		return errors.Errorf(
			context.Background(),
			"relay queue for one worker is full (%d items)",
			len(queue),
		)
	}
	s.byToken[token] = append(queue, body)
	s.total++
	s.mu.Unlock()
	s.signal()
	return nil
}

// Drain removes and returns everything queued.
func (s *Store) Drain() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total == 0 {
		return nil
	}
	items := make([]Item, 0, s.total)
	for token, queue := range s.byToken {
		for _, body := range queue {
			items = append(items, Item{Token: token, Body: body})
		}
	}
	s.byToken = make(map[string][]json.RawMessage)
	s.total = 0
	return items
}

// signal wakes one waiting pull without blocking when nobody is waiting.
func (s *Store) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Wait blocks until something is queued or the deadline passes. It returns no value: the
// caller re-drains, so a spurious wake is harmless and a missed wake costs one poll interval.
func (s *Store) Wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.notify:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// PushHandler accepts one worker push.
func PushHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token := r.Header.Get(TokenHeader)
		if token == "" {
			writeError(w, http.StatusBadRequest, "a push requires the "+TokenHeader+" header")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "the push body could not be read")
			return
		}
		if len(body) > MaxBodyBytes {
			writeError(
				w,
				http.StatusRequestEntityTooLarge,
				"the push body exceeds the relay's limit",
			)
			return
		}
		if !json.Valid(body) {
			writeError(w, http.StatusBadRequest, "the push body must be JSON")
			return
		}
		if err := store.Push(token, json.RawMessage(body)); err != nil {
			// 503, not 4xx: the worker did nothing wrong, and the SDK's sender retries on a
			// non-success status, which is what we want when the Mac is behind.
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
	})
}

// PullHandler serves the Mac's long-poll.
func PullHandler(store *Store, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		presented := r.Header.Get(TunnelSecretHeader)
		// Constant-time, and a wrong length must not short-circuit into a different status.
		if secret == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
			writeError(w, http.StatusUnauthorized, "unknown or missing tunnel secret")
			return
		}
		wait := parseWait(r.URL.Query().Get("wait"))
		ctx := r.Context()
		items := store.Drain()
		if len(items) == 0 {
			store.Wait(ctx, wait)
			items = store.Drain()
		}
		if items == nil {
			items = []Item{}
		}
		glog.V(2).Infof("relay pull returned %d item(s)", len(items))
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
}

// parseWait reads the caller's window, defaulting and clamping it.
func parseWait(raw string) time.Duration {
	if raw == "" {
		return DefaultWait
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultWait
	}
	if d > MaxWait {
		return MaxWait
	}
	return d
}

// writeJSON writes a JSON body with the canonical content type.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError writes a JSON error body. The message is a human sentence, never an internal
// error chain: this endpoint is reachable from inside the cluster and its output ends up in a
// worker's log.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
