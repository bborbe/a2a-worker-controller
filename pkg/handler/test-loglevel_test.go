// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package handler_test

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg/handler"
)

var _ = Describe("TestLoglevelHandler", func() {
	var httpHandler http.Handler

	BeforeEach(func() {
		httpHandler = handler.NewTestLoglevelHandler()
	})

	It("returns OK response", func() {
		req := httptest.NewRequest("GET", "/testloglevel", nil)
		resp := httptest.NewRecorder()

		httpHandler.ServeHTTP(resp, req)

		Expect(resp.Code).To(Equal(http.StatusOK))
		Expect(resp.Body.String()).To(ContainSubstring("test loglevel completed"))
	})
})
