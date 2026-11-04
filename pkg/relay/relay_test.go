// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg/relay"
)

func TestRelay(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Relay Suite")
}

// push builds a request the way the SDK's sender does: the token in its own header and a
// JSON body the relay never reads.
func push(token, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, relay.PushPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set(relay.TokenHeader, token)
	}
	return req
}

// pull builds an authenticated pull request.
func pull(secret string, wait time.Duration) *http.Request {
	req := httptest.NewRequest(http.MethodGet, relay.PullPath+"?wait="+wait.String(), nil)
	if secret != "" {
		req.Header.Set(relay.TunnelSecretHeader, secret)
	}
	return req
}

var _ = Describe("relay", func() {
	const secret = "s3cret"

	Describe("push", func() {
		It("accepts a token and a JSON body", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()

			relay.PushHandler(store).ServeHTTP(rec, push("tok-a", `{"kind":"statusUpdate"}`))

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(store.Drain()).To(HaveLen(1))
		})

		It("refuses a push with no token, because the token is the routing key", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()

			relay.PushHandler(store).ServeHTTP(rec, push("", `{}`))

			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(store.Drain()).To(BeEmpty())
		})

		It("refuses a body that is not JSON rather than buffering it", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()

			relay.PushHandler(store).ServeHTTP(rec, push("tok-a", `not json`))

			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(store.Drain()).To(BeEmpty())
		})

		It(
			"answers 503 rather than dropping when the queue is full, so the sender retries",
			func() {
				store := relay.NewStore(1)
				relay.PushHandler(store).ServeHTTP(httptest.NewRecorder(), push("tok-a", `{}`))
				rec := httptest.NewRecorder()

				relay.PushHandler(store).ServeHTTP(rec, push("tok-b", `{}`))

				Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			},
		)

		It("bounds one worker's queue even when the total has room", func() {
			store := relay.NewStore(1000)
			for i := 0; i < relay.MaxPerToken; i++ {
				rec := httptest.NewRecorder()
				relay.PushHandler(store).ServeHTTP(rec, push("tok-a", `{}`))
				Expect(rec.Code).To(Equal(http.StatusOK))
			}
			rec := httptest.NewRecorder()

			relay.PushHandler(store).ServeHTTP(rec, push("tok-a", `{}`))

			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		})
	})

	Describe("pull", func() {
		It("refuses a missing secret, so a worker cannot drain the queue", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()

			relay.PullHandler(store, secret).ServeHTTP(rec, pull("", time.Millisecond))

			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		})

		It("refuses a wrong secret", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()

			relay.PullHandler(store, secret).ServeHTTP(rec, pull("wrong", time.Millisecond))

			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		})

		It("returns queued items with the token they were routed by", func() {
			store := relay.NewStore(10)
			relay.PushHandler(store).ServeHTTP(httptest.NewRecorder(), push("tok-a", `{"a":1}`))
			rec := httptest.NewRecorder()

			relay.PullHandler(store, secret).ServeHTTP(rec, pull(secret, time.Millisecond))

			Expect(rec.Code).To(Equal(http.StatusOK))
			var body struct {
				Items []relay.Item `json:"items"`
			}
			Expect(json.NewDecoder(rec.Body).Decode(&body)).To(Succeed())
			Expect(body.Items).To(HaveLen(1))
			Expect(body.Items[0].Token).To(Equal("tok-a"))
			Expect(string(body.Items[0].Body)).To(Equal(`{"a":1}`))
		})

		It("drains, so a second pull does not deliver the same status twice", func() {
			store := relay.NewStore(10)
			relay.PushHandler(store).ServeHTTP(httptest.NewRecorder(), push("tok-a", `{}`))
			relay.PullHandler(store, secret).
				ServeHTTP(httptest.NewRecorder(), pull(secret, time.Millisecond))
			rec := httptest.NewRecorder()

			relay.PullHandler(store, secret).ServeHTTP(rec, pull(secret, time.Millisecond))

			var body struct {
				Items []relay.Item `json:"items"`
			}
			Expect(json.NewDecoder(rec.Body).Decode(&body)).To(Succeed())
			Expect(body.Items).To(BeEmpty())
		})

		It(
			"answers an empty poll with an empty list rather than null, so the client can decode it",
			func() {
				store := relay.NewStore(10)
				rec := httptest.NewRecorder()

				relay.PullHandler(store, secret).ServeHTTP(rec, pull(secret, time.Millisecond))

				Expect(rec.Code).To(Equal(http.StatusOK))
				Expect(strings.TrimSpace(rec.Body.String())).To(Equal(`{"items":[]}`))
			},
		)

		It("returns as soon as something is queued instead of waiting out the window", func() {
			store := relay.NewStore(10)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				relay.PullHandler(store, secret).ServeHTTP(rec, pull(secret, 10*time.Second))
			}()

			// The pull is blocked in its wait; a push must release it well inside the window.
			time.Sleep(50 * time.Millisecond)
			relay.PushHandler(store).ServeHTTP(httptest.NewRecorder(), push("tok-a", `{}`))

			Eventually(done, 2*time.Second).Should(BeClosed())
			Expect(rec.Code).To(Equal(http.StatusOK))
		})

		It("stops waiting when the caller's context ends, so a dead client pins nothing", func() {
			store := relay.NewStore(10)
			ctx, cancel := context.WithCancel(context.Background())
			req := pull(secret, 10*time.Second).WithContext(ctx)
			cancel()

			start := time.Now()
			rec := httptest.NewRecorder()
			relay.PullHandler(store, secret).ServeHTTP(rec, req)

			Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
			Expect(rec.Code).To(Equal(http.StatusOK))
		})
	})
})
