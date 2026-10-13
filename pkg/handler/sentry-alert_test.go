// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package handler_test

import (
	"net/http"
	"net/http/httptest"

	sentrymocks "github.com/bborbe/sentry/mocks"
	"github.com/getsentry/sentry-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg/handler"
)

var _ = Describe("SentryAlertHandler", func() {
	var (
		httpHandler  http.Handler
		sentryClient *sentrymocks.SentryClient
	)

	BeforeEach(func() {
		sentryClient = &sentrymocks.SentryClient{}
		eventID := sentry.EventID("test-event-id")
		sentryClient.CaptureExceptionReturns(&eventID)
		httpHandler = handler.NewSentryAlertHandler(sentryClient)
	})

	It("captures exception to Sentry", func() {
		req := httptest.NewRequest("GET", "/sentryalert", nil)
		resp := httptest.NewRecorder()

		httpHandler.ServeHTTP(resp, req)

		Expect(resp.Code).To(Equal(http.StatusOK))
		Expect(sentryClient.CaptureExceptionCallCount()).To(Equal(1))
	})
})
