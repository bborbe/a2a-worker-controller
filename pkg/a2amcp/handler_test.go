// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg/a2amcp"
)

// testBearer is the local bearer every spec's request presents unless it says otherwise.
const testBearer = "test-bearer-0123456789abcdef"

var _ = Describe("Handler", func() {
	var (
		client  *mocks.A2amcpClient
		handler http.Handler
		resp    *httptest.ResponseRecorder
	)

	newRequest := func(body string) *http.Request {
		req := httptest.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:8090/mcp",
			strings.NewReader(body),
		)
		req.RemoteAddr = "127.0.0.1:51234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testBearer)
		req.Header.Set(a2amcp.SessionHeader, "session-1")
		return req
	}
	rpc := func() rpcReply {
		var payload rpcReply
		Expect(json.Unmarshal(resp.Body.Bytes(), &payload)).To(Succeed())
		return payload
	}
	toolText := func() (string, bool) {
		result := rpc().Result
		Expect(result.Content).To(HaveLen(1))
		return result.Content[0].Text, result.IsError
	}

	BeforeEach(func() {
		client = &mocks.A2amcpClient{}
		handler = a2amcp.NewHandler(client, testBearer)
		resp = httptest.NewRecorder()
	})

	Context("gates", func() {
		const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

		It(
			"answers 200 for a loopback caller with the bearer naming its session (control)",
			func() {
				handler.ServeHTTP(resp, newRequest(listTools))
				Expect(resp.Code).To(Equal(http.StatusOK))
			},
		)

		It("refuses a missing bearer with 401 even from loopback", func() {
			req := newRequest(listTools)
			req.Header.Del("Authorization")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusUnauthorized))
			Expect(resp.Header().Get("WWW-Authenticate")).To(Equal("Bearer"))
			Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ReasonMissingBearer))
		})

		It("refuses a wrong bearer with 401 without echoing it", func() {
			req := newRequest(listTools)
			req.Header.Set("Authorization", "Bearer forged-by-a-worker")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusUnauthorized))
			Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ReasonInvalidBearer))
			Expect(resp.Body.String()).NotTo(ContainSubstring("forged-by-a-worker"))
		})

		It("refuses every call when built without a bearer", func() {
			handler = a2amcp.NewHandler(client, "")
			req := newRequest(listTools)
			req.Header.Set("Authorization", "Bearer ")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusUnauthorized))
		})

		It("answers refusals in the controller's structured error shape", func() {
			req := newRequest(listTools)
			req.Header.Del("Authorization")
			handler.ServeHTTP(resp, req)
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			Expect(json.Unmarshal(resp.Body.Bytes(), &body)).To(Succeed())
			Expect(body.Error.Code).To(Equal(a2amcp.ReasonMissingBearer))
			Expect(body.Error.Message).To(ContainSubstring("Bearer"))
		})

		It("refuses a connection that arrived on a non-loopback local address with 403", func() {
			req := newRequest(listTools)
			localAddr := &net.TCPAddr{IP: net.ParseIP("192.168.178.20"), Port: 8090}
			req = req.WithContext(
				context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr),
			)
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusForbidden))
			Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ReasonNonLoopback))
		})

		It("refuses an oversized body with 413", func() {
			handler.ServeHTTP(resp, newRequest(`{"pad":"`+strings.Repeat("x", 1<<20)+`"}`))
			Expect(resp.Code).To(Equal(http.StatusRequestEntityTooLarge))
		})

		It("refuses a GET with 405 naming the allowed methods", func() {
			req := newRequest("")
			req.Method = http.MethodGet
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(resp.Header().Get("Allow")).To(Equal("POST, DELETE"))
		})

		It(
			"serves a call with no session header, so the handshake still registers the tools",
			func() {
				req := newRequest(listTools)
				req.Header.Del(a2amcp.SessionHeader)
				handler.ServeHTTP(resp, req)
				Expect(resp.Code).To(Equal(http.StatusOK))
				Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ToolAgentCard))
			},
		)

		It("serves a call whose session header is blank", func() {
			req := newRequest(listTools)
			req.Header.Set(a2amcp.SessionHeader, "  ")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusOK))
		})

		It("refuses a non-loopback peer with 403 even with a session header", func() {
			req := newRequest(listTools)
			req.RemoteAddr = "192.168.178.20:51234"
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusForbidden))
			Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ReasonNonLoopback))
		})

		It("refuses a non-loopback Host with 403", func() {
			req := newRequest(listTools)
			req.Host = "host.docker.internal:8090"
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusForbidden))
			Expect(resp.Body.String()).To(ContainSubstring(a2amcp.ReasonOriginNotAllowed))
		})

		It("refuses a non-loopback browser Origin with 403", func() {
			req := newRequest(listTools)
			req.Header.Set("Origin", "https://evil.example")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusForbidden))
		})

		It("refuses a POST without a JSON content type with 415", func() {
			req := newRequest(listTools)
			req.Header.Del("Content-Type")
			handler.ServeHTTP(resp, req)
			Expect(resp.Code).To(Equal(http.StatusUnsupportedMediaType))
		})
	})

	Context("JSON-RPC", func() {
		It("echoes the client's protocol version on initialize", func() {
			handler.ServeHTTP(resp, newRequest(
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
			))
			Expect(rpc().Result.ProtocolVersion).To(Equal("2025-03-26"))
		})

		It("lists exactly the shim's two tools", func() {
			handler.ServeHTTP(resp, newRequest(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			tools := rpc().Result.Tools
			names := make([]string, 0, len(tools))
			for _, tool := range tools {
				names = append(names, tool.Name)
			}
			Expect(names).To(ConsistOf(a2amcp.ToolAgentCard, a2amcp.ToolSend))
		})

		It("answers a notification with 202 and no body", func() {
			handler.ServeHTTP(
				resp,
				newRequest(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
			)
			Expect(resp.Code).To(Equal(http.StatusAccepted))
			Expect(resp.Body.Len()).To(Equal(0))
		})

		It("reports an unknown method as -32601", func() {
			handler.ServeHTTP(resp, newRequest(`{"jsonrpc":"2.0","id":1,"method":"nope"}`))
			Expect(rpc().Error).NotTo(BeNil())
			Expect(rpc().Error.Code).To(Equal(-32601))
		})
	})

	Context("tools/call", func() {
		It("passes text and context_id to the client and returns its result", func() {
			client.SendReturns(map[string]any{"reply": "pong"}, nil)
			handler.ServeHTTP(resp, newRequest(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a2a_send","arguments":{"text":"ping","context_id":"c1"}}}`,
			))
			Expect(client.SendCallCount()).To(Equal(1))
			_, text, contextID := client.SendArgsForCall(0)
			Expect(text).To(Equal("ping"))
			Expect(contextID).To(Equal("c1"))
			body, isError := toolText()
			Expect(isError).To(BeFalse())
			Expect(body).To(Equal(`{"reply":"pong"}`))
		})

		It("refuses a2a_send without text and never calls the client", func() {
			handler.ServeHTTP(resp, newRequest(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a2a_send","arguments":{}}}`,
			))
			Expect(client.SendCallCount()).To(Equal(0))
			_, isError := toolText()
			Expect(isError).To(BeTrue())
		})

		It("returns a client failure as a tool error", func() {
			client.AgentCardReturns(nil, errors.New("card unreachable"))
			handler.ServeHTTP(resp, newRequest(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a2a_agent_card"}}`,
			))
			body, isError := toolText()
			Expect(isError).To(BeTrue())
			Expect(body).To(ContainSubstring("card unreachable"))
		})

		It("reports an unknown tool as -32602", func() {
			handler.ServeHTTP(resp, newRequest(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`,
			))
			Expect(rpc().Error).NotTo(BeNil())
			Expect(rpc().Error.Code).To(Equal(-32602))
		})
	})
})

// rpcReply is the slice of a JSON-RPC response these specs assert on.
type rpcReply struct {
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Tools           []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}
