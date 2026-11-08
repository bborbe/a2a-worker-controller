// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp

import (
	"encoding/json"
	"net/http"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type sendArgs struct {
	Text      string `json:"text"`
	ContextID string `json:"context_id"`
}

func writeRPC(resp http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	resp.Header().Set("Content-Type", "application/json")
	payload := rpcResponse{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		payload.Error = rpcErr
	} else {
		payload.Result = result
	}
	resp.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(resp).Encode(payload)
}

// initializeResult echoes the client's protocol version: every published revision shares the
// subset served here, and a client handed a version it did not ask for may refuse the session.
func initializeResult(params json.RawMessage) map[string]any {
	version := mcpProtocolVersion
	var body struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &body); err == nil && body.ProtocolVersion != "" {
		version = body.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "a2a", "version": "1.0.0"},
	}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

// toolDefinitions describes the two tools exactly as the stdio shim did.
func toolDefinitions() []any {
	return []any{
		map[string]any{
			"name":        ToolAgentCard,
			"description": "Fetch the remote agent's A2A Agent Card (public; no credential).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name": ToolSend,
			"description": "Send one message to the remote agent over A2A and return its reply. " +
				"The endpoint is read from the Agent Card, never configured separately. " +
				"context_id selects the conversation on the remote agent (empty = its default conversation).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":       map[string]any{"type": "string"},
					"context_id": map[string]any{"type": "string", "default": ""},
				},
				"required": []any{"text"},
			},
		},
	}
}
