// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package handler

import (
	"encoding/json"
	"net/http"

	libhttp "github.com/bborbe/http"
)

// NewHealthzHandler creates an HTTP handler that serves the canonical liveness
// response. It returns HTTP 200 with body `{"status":"ok"}` and
// Content-Type: application/json. The handler is dependency-free: it never
// touches Kafka, BoltDB, Sentry, or any other subsystem, so it can only ever
// fail by reporting "the process is down" (process exit / listener gone).
func NewHealthzHandler() http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Add(libhttp.ContentTypeHeaderName, libhttp.ApplicationJSONContentType)
		// Use json.Marshal + write raw bytes instead of json.NewEncoder.Encode
		// so the response body is exactly {"status":"ok"} with no trailing newline.
		body, err := json.Marshal(struct {
			Status string `json:"status"`
		}{Status: "ok"})
		if err != nil {
			// Marshal of a static struct literal cannot fail in practice; if it
			// ever does, returning an empty 500 would break the liveness probe.
			// Fall back to the exact bytes which are guaranteed valid JSON.
			body = []byte(`{"status":"ok"}`)
		}
		_, _ = resp.Write(body)
	})
}
