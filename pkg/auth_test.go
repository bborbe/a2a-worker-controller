// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// testToken is a fixture, not a credential: it is short, not hex, and appears only here. A
// token-shaped literal would defeat the probe that checks no credential reaches this repo.
const testToken = "test-token"

var _ = Describe("Auth", func() {
	// reached records whether the wrapped handler ran, which is what distinguishes a refusal
	// from a served request even when both answer with a status code.
	var reached bool

	// serve drives one request through the gate around a recording handler and returns the
	// recorder the gate wrote to.
	serve := func(auth pkg.Auth, method string, path string, authorization string) *httptest.ResponseRecorder {
		reached = false
		handler := auth.Wrap(http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
			reached = true
			resp.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(method, path, nil)
		if authorization != "" {
			req.Header.Set(pkg.AuthorizationHeader, authorization)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	Context("on a gated route", func() {
		It("serves a request presenting the token", func() {
			rec := serve(
				pkg.NewAuthToken(testToken),
				http.MethodPost,
				pkg.A2APath,
				"Bearer "+testToken,
			)
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(reached).To(BeTrue())
		})

		It("refuses a request carrying no credential", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodPost, pkg.A2APath, "")
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
			Expect(rec.Header().Get("WWW-Authenticate")).To(Equal("Bearer"))
		})

		It("refuses a request carrying a different token", func() {
			rec := serve(
				pkg.NewAuthToken(testToken),
				http.MethodPost,
				pkg.A2APath,
				"Bearer other-token",
			)
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("refuses a request whose token is a prefix of the real one", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodPost, pkg.A2APath, "Bearer test")
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("refuses a request presenting the token under another scheme", func() {
			rec := serve(
				pkg.NewAuthToken(testToken),
				http.MethodPost,
				pkg.A2APath,
				"Basic "+testToken,
			)
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("refuses a request presenting the token with no scheme at all", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodPost, pkg.A2APath, testToken)
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("answers a refusal with the service's structured error shape", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodPost, pkg.A2APath, "")
			Expect(rec.Header().Get("Content-Type")).To(Equal("application/json"))
			Expect(
				rec.Body.String(),
			).To(MatchJSON(`{"error":{"code":"UNAUTHORIZED","message":"unauthorized","details":{}}}`))
		})

		It("gates the A2A Agent Card too", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodGet, pkg.AgentCardPath, "")
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("gates a path that carries no route at all", func() {
			rec := serve(pkg.NewAuthToken(testToken), http.MethodGet, "/not-a-route", "")
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})
	})

	Context("on an exempt admin path", func() {
		DescribeTable("serves it without a credential",
			func(path string) {
				rec := serve(pkg.NewAuthToken(testToken), http.MethodGet, path, "")
				Expect(rec.Code).To(Equal(http.StatusOK))
				Expect(reached).To(BeTrue())
			},
			Entry("healthz", "/healthz"),
			Entry("readiness", "/readiness"),
			Entry("metrics", "/metrics"),
			Entry("gc", "/gc"),
			Entry("setloglevel", "/setloglevel/debug"),
		)
	})

	Context("with no usable token", func() {
		It("refuses every gated request when built from the zero value", func() {
			rec := serve(pkg.Auth{}, http.MethodPost, pkg.A2APath, "Bearer "+testToken)
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("refuses every gated request when the token is empty", func() {
			rec := serve(pkg.NewAuthToken(""), http.MethodPost, pkg.A2APath, "Bearer ")
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(reached).To(BeFalse())
		})

		It("still serves the exempt admin paths", func() {
			rec := serve(pkg.Auth{}, http.MethodGet, "/healthz", "")
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(reached).To(BeTrue())
		})
	})
})
