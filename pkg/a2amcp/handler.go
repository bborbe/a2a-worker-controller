// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package a2amcp serves the `a2a` MCP tools over HTTP, replacing the per-session stdio shim
// (a2a-mcp.py) every Claude Code session used to start for itself.
//
// The tools are an A2A *client*: they reach an operator-configured target agent, not this
// controller's own /a2a surface. The controller only hosts them because it is a long-running
// KeepAlive process on the operator's machine.
package a2amcp

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// Path is the route the MCP endpoint is served at. It is declared in pkg, beside the bearer
// gate's exemption list, so the two cannot drift apart.
const Path = pkg.MCPPath

// SessionHeader carries the calling Claude Code session's id. Claude Code's headersHelper
// (mcp-headers-helper.sh) sets it; it is the same header the claude-wrapper MCP faces use.
const SessionHeader = "X-Claude-Code-Session-Id"

// The refusal reasons a log line and a 4xx body carry.
const (
	ReasonMissingSessionHeader = "missing_session_header"
	ReasonEmptySessionHeader   = "empty_session_header"
	ReasonNonLoopback          = "non_loopback"
	ReasonOriginNotAllowed     = "origin_not_allowed"
)

// mcpProtocolVersion is offered when the client names none.
const mcpProtocolVersion = "2025-06-18"

// maxRequestBytes caps one JSON-RPC request body.
const maxRequestBytes = 1 << 20

// Tool names, identical to the stdio shim's so no caller changes.
const (
	ToolAgentCard = "a2a_agent_card"
	ToolSend      = "a2a_send"
)

// NewHandler returns the MCP endpoint backed by client.
//
// ⚠️ This route is exempt from the controller's bearer gate and gated here instead: it must be
// reachable by every local session without each holding the controller's token, yet it sends
// A2A with the operator's token. So it refuses any caller not on loopback, and any call that
// does not name its session — a call is attributed, never defaulted.
func NewHandler(client Client) http.Handler {
	return &handler{client: client}
}

type handler struct {
	client Client
}

func (h *handler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	remote := req.RemoteAddr
	if !loopbackCaller(req) {
		refuse(ctx, resp, http.StatusForbidden, ReasonNonLoopback, remote,
			"the a2a MCP endpoint serves loopback callers only")
		return
	}
	if !originAllowed(req) {
		refuse(ctx, resp, http.StatusForbidden, ReasonOriginNotAllowed, remote,
			"a browser origin that is not loopback may not call the a2a MCP endpoint")
		return
	}
	values := req.Header.Values(SessionHeader)
	if len(values) == 0 {
		refuse(
			ctx,
			resp,
			http.StatusBadRequest,
			ReasonMissingSessionHeader,
			remote,
			"missing "+SessionHeader+" header — a call is attributed to its session, so it is refused rather than defaulted",
		)
		return
	}
	sessionID := strings.TrimSpace(values[0])
	if sessionID == "" {
		refuse(
			ctx,
			resp,
			http.StatusBadRequest,
			ReasonEmptySessionHeader,
			remote,
			"empty "+SessionHeader+" header — a call is attributed to its session, so it is refused rather than defaulted",
		)
		return
	}
	switch req.Method {
	case http.MethodPost:
		if !isJSONRequest(req) {
			resp.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		h.handlePost(resp, req, sessionID)
	case http.MethodDelete:
		// Session termination: there is no per-session state to drop.
		resp.WriteHeader(http.StatusOK)
	default:
		resp.Header().Set("Allow", "POST, DELETE")
		resp.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *handler) handlePost(resp http.ResponseWriter, req *http.Request, sessionID string) {
	ctx := req.Context()
	// An A2A turn may run for minutes, past the server's write timeout; a reply written after
	// that deadline never reaches the client. Best effort: a recorder under test has none.
	if err := http.NewResponseController(resp).SetWriteDeadline(time.Time{}); err != nil &&
		!stderrors.Is(err, http.ErrNotSupported) {
		slog.WarnContext(ctx, "mcp clear write deadline failed", "error", err)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBytes+1))
	if err != nil {
		writeRPC(resp, nil, nil, &rpcError{Code: -32700, Message: "read body failed"})
		return
	}
	if len(body) > maxRequestBytes {
		resp.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var request rpcRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeRPC(resp, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	tool := toolName(request)
	slog.InfoContext(ctx, fmt.Sprintf(
		"mcp call session=%s method=%s tool=%s remote=%s",
		sessionID, request.Method, tool, req.RemoteAddr,
	))
	result, rpcErr := h.dispatch(ctx, &request)
	// A notification carries no id and is never answered.
	if len(request.ID) == 0 {
		resp.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(resp, request.ID, result, rpcErr)
}

func (h *handler) dispatch(ctx context.Context, request *rpcRequest) (any, *rpcError) {
	switch request.Method {
	case "initialize":
		return initializeResult(request.Params), nil
	case "notifications/initialized":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil
	case "tools/call":
		return h.callTool(ctx, request.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + request.Method}
	}
}

func (h *handler) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var call toolCall
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	var (
		result map[string]any
		err    error
	)
	switch call.Name {
	case ToolAgentCard:
		result, err = h.client.AgentCard(ctx)
	case ToolSend:
		var args sendArgs
		if len(call.Arguments) > 0 {
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return toolResult("invalid arguments: "+err.Error(), true), nil
			}
		}
		if args.Text == "" {
			return toolResult("text is required", true), nil
		}
		result, err = h.client.Send(ctx, args.Text, args.ContextID)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
	}
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return toolResult("encode result failed", true), nil
	}
	return toolResult(string(encoded), false), nil
}

// refuse answers a gated request and logs the refusal in its pinned format.
func refuse(
	ctx context.Context,
	resp http.ResponseWriter,
	status int,
	reason string,
	remote string,
	message string,
) {
	slog.WarnContext(ctx, fmt.Sprintf("mcp refused reason=%s remote=%s", reason, remote))
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(status)
	_ = json.NewEncoder(resp).Encode(map[string]string{"error": reason, "message": message})
}

// loopbackCaller reports whether the request both came from and arrived on loopback.
//
// The peer address alone is not enough: the controller binds every interface, and the local
// address is what the connection actually reached. A recorder under test carries no local
// address, which is why that half is conditional.
func loopbackCaller(req *http.Request) bool {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil || !isLoopbackIP(host) {
		return false
	}
	if addr, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		localHost, _, err := net.SplitHostPort(addr.String())
		if err != nil || !isLoopbackIP(localHost) {
			return false
		}
	}
	return true
}

// originAllowed rejects a request whose Host is not a loopback name, and one whose Origin
// names a non-loopback host — the DNS-rebinding guard the claude-wrapper MCP faces apply. An
// absent Origin is allowed: a non-browser client sends none.
//
// The Host check also refuses a container reaching the host as `host.docker.internal`, which
// Docker Desktop may deliver from a loopback peer address.
func originAllowed(req *http.Request) bool {
	if !isLoopbackName(req.Host) {
		return false
	}
	origin := req.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackName(parsed.Host)
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isLoopbackName reports whether host (with or without a port) is a loopback name.
func isLoopbackName(host string) bool {
	name := host
	if withoutPort, _, err := net.SplitHostPort(host); err == nil {
		name = withoutPort
	}
	name = strings.ToLower(strings.Trim(name, "[]"))
	return name == "localhost" || isLoopbackIP(name)
}

// isJSONRequest rejects an absent Content-Type too: a POST without one is a CORS simple
// request, and accepting it would reopen the preflight bypass the Origin check closes.
func isJSONRequest(req *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func toolName(request rpcRequest) string {
	if request.Method != "tools/call" {
		return "-"
	}
	var call toolCall
	if err := json.Unmarshal(request.Params, &call); err != nil || call.Name == "" {
		return "-"
	}
	return call.Name
}
