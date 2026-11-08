// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg/a2amcp"
)

var _ = Describe("Client", func() {
	var (
		ctx        context.Context
		server     *httptest.Server
		tokens     *mocks.A2amcpTokenSource
		sendStatus int
		sendBody   map[string]any
		gotAuth    string
		gotVersion string
		gotRequest *sentRequest
	)

	BeforeEach(func() {
		ctx = context.Background()
		tokens = &mocks.A2amcpTokenSource{}
		tokens.TokenReturns("secret-token", nil)
		gotAuth = ""
		gotVersion = ""
		gotRequest = nil
		sendStatus = http.StatusOK
		sendBody = map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{"task": map[string]any{
				"id":        "t1",
				"contextId": "c1",
				"status":    map[string]any{"state": "TASK_STATE_COMPLETED"},
				"artifacts": []any{map[string]any{"parts": []any{
					map[string]any{"text": "pong"},
					map[string]any{"text": "again"},
				}}},
			}},
		}
		mux := http.NewServeMux()
		mux.HandleFunc(a2amcp.AgentCardPath, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":                "claude-interactive-local",
				"supportedInterfaces": []any{map[string]any{"url": server.URL + "/a2a"}},
				"skills":              []any{map[string]any{"id": "chat"}},
			})
		})
		mux.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotVersion = r.Header.Get(a2amcp.A2AVersionHeader)
			gotRequest = &sentRequest{}
			_ = json.NewDecoder(r.Body).Decode(gotRequest)
			w.WriteHeader(sendStatus)
			_ = json.NewEncoder(w).Encode(sendBody)
		})
		server = httptest.NewServer(mux)
	})

	AfterEach(func() {
		server.Close()
	})

	newClient := func() a2amcp.Client {
		return a2amcp.NewClient(server.Client(), server.URL, tokens)
	}

	It("summarises the Agent Card like the shim", func() {
		card, err := newClient().AgentCard(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(card["name"]).To(Equal("claude-interactive-local"))
		Expect(card["url"]).To(Equal(server.URL + "/a2a"))
		Expect(card["skills"]).To(Equal([]any{"chat"}))
		Expect(tokens.TokenCallCount()).To(Equal(0))
	})

	It("sends SendMessage with the bearer token and flattens the reply", func() {
		result, err := newClient().Send(ctx, "ping", "c1")
		Expect(err).NotTo(HaveOccurred())
		Expect(gotAuth).To(Equal("Bearer secret-token"))
		Expect(gotVersion).To(Equal(a2amcp.A2AProtocolVersion))
		Expect(gotRequest).NotTo(BeNil())
		Expect(gotRequest.Method).To(Equal("SendMessage"))
		message := gotRequest.Params.Message
		Expect(message["role"]).To(Equal("ROLE_USER"))
		Expect(message["contextId"]).To(Equal("c1"))
		Expect(message["messageId"]).NotTo(BeEmpty())
		Expect(result["reply"]).To(Equal("pong\nagain"))
		Expect(result["task_id"]).To(Equal("t1"))
		Expect(result["context_id"]).To(Equal("c1"))
		Expect(result["state"]).To(Equal("TASK_STATE_COMPLETED"))
		Expect(result["http_status"]).To(Equal(http.StatusOK))
	})

	It("omits contextId when none is given", func() {
		_, err := newClient().Send(ctx, "ping", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(gotRequest).NotTo(BeNil())
		Expect(gotRequest.Params.Message).NotTo(HaveKey("contextId"))
	})

	It("reports a non-200 SendMessage as http_status only", func() {
		sendStatus = http.StatusUnauthorized
		result, err := newClient().Send(ctx, "ping", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(map[string]any{"http_status": http.StatusUnauthorized}))
	})

	It("reports a JSON-RPC error as jsonrpc_error", func() {
		sendBody = map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]any{"code": -32000.0},
		}
		result, err := newClient().Send(ctx, "ping", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result["jsonrpc_error"]).To(Equal(map[string]any{"code": -32000.0}))
	})

	It("fails before contacting the agent when no token resolves", func() {
		tokens.TokenReturns("", errors.New("failed to resolve A2A token"))
		_, err := newClient().Send(ctx, "ping", "")
		Expect(err).To(HaveOccurred())
		Expect(gotRequest).To(BeNil())
	})
})

// sentRequest is the SendMessage request the fake agent received.
type sentRequest struct {
	Method string `json:"method"`
	Params struct {
		Message map[string]any `json:"message"`
	} `json:"params"`
}
