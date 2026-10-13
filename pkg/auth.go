// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// The bearer gate's header and scheme. A caller presents the controller's token as
// `Authorization: Bearer <token>`, the scheme the A2A binding and every sibling service use.
const (
	// AuthorizationHeader is the request header carrying the caller's bearer credential.
	AuthorizationHeader = "Authorization"
	// bearerPrefix is the authorization scheme this controller accepts, with the space that
	// separates it from the credential.
	bearerPrefix = "Bearer "
	// bearerChallenge is the WWW-Authenticate value a refusal advertises. It is the scheme
	// alone: a challenge carrying no parameters must not end in the separator space.
	bearerChallenge = "Bearer"
)

// The admin paths served without authentication, and the log-level route's prefix.
//
// /healthz and /readiness are exempt because a kubelet liveness and readiness probe cannot
// present a bearer credential without the token being written into the pod spec's probe
// stanza, which multiplies the secret's exposure and can wedge the pod's Ready state.
// /metrics is exempt for the same reason on the scrape side: a Prometheus job would need the
// token in its own configuration. /setloglevel/{level} and /gc are exempt as the five
// canonical admin endpoints, reached through the gateway's human-only mount rather than by an
// agent. None of the five grants a control operation and none reveals a credential.
//
// The A2A Agent Card is deliberately NOT exempt. The reference gate in
// github.com/bborbe/agent/interactive exempts it as public discovery, but every caller of
// this controller already holds the token, so gating the card costs a caller nothing and
// keeps the surface that creates and destroys containers closed by default.
const (
	healthzPath           = "/healthz"
	readinessPath         = "/readiness"
	metricsPath           = "/metrics"
	setLogLevelPathPrefix = "/setloglevel/"
	gcPath                = "/gc"
)

// unauthorizedErrorCode is the code the gate's 401 body carries.
const unauthorizedErrorCode = "UNAUTHORIZED"

// Auth is the controller's authentication decision: the bearer token every gated request
// must present.
//
// The zero value is not a usable default — it refuses every gated request, so a controller
// built without a token serves only the exempt admin paths. That is the fail-closed outcome:
// a decision left unmade cannot silently open a surface that creates and destroys containers.
// There is deliberately no opt-out value. The reference gate carries one so a deliberately
// unexposed consumer can state its choice; this controller has no deployment in which serving
// /a2a unauthenticated is correct, so the escape hatch is omitted rather than left unused.
type Auth struct {
	token string
}

// NewAuthToken returns the Auth that requires token as the bearer credential on every gated
// request. An empty token is not a usable credential: it yields the same fail-closed value as
// the zero Auth.
func NewAuthToken(token string) Auth {
	return Auth{token: token}
}

// Wrap returns the handler that serves next only to a request carrying this decision's bearer
// credential.
//
// It wraps the whole router rather than individual routes, so /a2a and any route registered
// after this change are covered by construction rather than by remembering to add the gate.
// A refused request is answered before the wrapped handler runs, so it never reaches the
// executor.
func (a Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if authExempt(req.URL.Path) || a.allows(req.Header.Get(AuthorizationHeader)) {
			next.ServeHTTP(resp, req)
			return
		}
		writeUnauthorized(resp)
	})
}

// allows reports whether header carries this decision's bearer credential.
//
// The comparison is constant-time: subtle.ConstantTimeCompare returns 0 when the lengths
// differ and compares every byte otherwise, so a caller cannot recover the token's length or
// prefix from response timing. The scheme prefix is not secret and is compared with an
// ordinary prefix test.
func (a Auth) allows(header string) bool {
	if a.token == "" {
		return false
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	presented := strings.TrimPrefix(header, bearerPrefix)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) == 1
}

// authExempt reports whether path is served without authentication. The log-level route is
// matched by prefix because its level is a path variable, so an exact match would exempt
// nothing and the endpoint would be gated while its callers believe it is not.
func authExempt(path string) bool {
	switch path {
	case healthzPath, readinessPath, metricsPath, gcPath:
		return true
	default:
		return strings.HasPrefix(path, setLogLevelPathPrefix)
	}
}

// writeUnauthorized answers a refused request. The body is the service's structured error
// shape rather than the plain-text body the reference gate writes, so a caller parsing this
// service's responses needs no second parser for its refusals.
func writeUnauthorized(resp http.ResponseWriter) {
	resp.Header().Set("WWW-Authenticate", bearerChallenge)
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(resp).Encode(map[string]any{
		"error": map[string]any{
			"code":    unauthorizedErrorCode,
			"message": "unauthorized",
			"details": map[string]string{},
		},
	})
}
