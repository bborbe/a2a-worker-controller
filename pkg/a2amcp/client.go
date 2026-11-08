// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bborbe/errors"
	"github.com/google/uuid"
)

// AgentCardPath is the well-known path the target's Agent Card is served at.
const AgentCardPath = "/.well-known/agent-card.json"

// DefaultTimeout bounds one call to the target agent. It matches the stdio shim's 300 s read
// budget: an A2A turn is a full model turn and routinely takes minutes.
const DefaultTimeout = 300 * time.Second

// maxResponseBytes caps what is read from the target, so a misbehaving agent cannot exhaust
// the controller's memory through a tool call.
const maxResponseBytes = 8 << 20

//counterfeiter:generate -o ../../mocks/a2amcp-client.go --fake-name A2amcpClient . Client

// Client is the A2A client behind the two MCP tools. Both methods return the same result
// shapes the stdio shim (a2a-mcp.py) returned, field for field, so a caller sees no change.
type Client interface {
	// AgentCard fetches the target's Agent Card. The card is public: no credential is sent.
	AgentCard(ctx context.Context) (map[string]any, error)
	// Send posts one user message to the endpoint the card advertises and returns its reply.
	// contextID selects the remote conversation; empty means the agent's default.
	Send(ctx context.Context, text string, contextID string) (map[string]any, error)
}

// NewClient returns the Client that talks to the agent at baseURL.
func NewClient(httpClient *http.Client, baseURL string, tokens TokenSource) Client {
	return &client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
	}
}

type client struct {
	httpClient *http.Client
	baseURL    string
	tokens     TokenSource
}

func (c *client) AgentCard(ctx context.Context) (map[string]any, error) {
	card, err := c.card(ctx)
	if err != nil {
		return nil, err
	}
	skills := []any{}
	for _, skill := range listOf(card["skills"]) {
		if entry, ok := skill.(map[string]any); ok {
			skills = append(skills, entry["id"])
		}
	}
	return map[string]any{
		"name":   card["name"],
		"url":    interfaceURL(card),
		"skills": skills,
	}, nil
}

func (c *client) Send(ctx context.Context, text string, contextID string) (map[string]any, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	card, err := c.card(ctx)
	if err != nil {
		return nil, err
	}
	endpoint, _ := interfaceURL(card).(string)
	if endpoint == "" {
		return map[string]any{"error": "Agent Card advertises no interface URL"}, nil
	}
	message := map[string]any{
		"messageId": uuid.NewString(),
		"role":      "ROLE_USER",
		"parts":     []any{map[string]any{"text": text}},
	}
	if contextID != "" {
		message["contextId"] = contextID
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  map[string]any{"message": message},
	})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "marshal SendMessage failed")
	}
	// The endpoint is the one the operator-configured target's own card advertises — the same
	// trust the stdio shim placed in it.
	//nolint:gosec // G704: target is operator configuration, not caller input
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "build SendMessage request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	//nolint:gosec // G704: target is operator configuration, not caller input
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// The error names the endpoint, never the request headers, so the token cannot leak.
		return nil, errors.Wrapf(ctx, err, "SendMessage to %s failed", endpoint)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return map[string]any{"http_status": resp.StatusCode}, nil
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, errors.Wrapf(ctx, err, "decode SendMessage response failed")
	}
	if rpcErr, ok := payload["error"]; ok {
		return map[string]any{"http_status": http.StatusOK, "jsonrpc_error": rpcErr}, nil
	}
	return sendResult(endpoint, payload), nil
}

// sendResult flattens a SendMessage response the way the shim did: the task is result.task
// when present, else result itself; the reply joins every artifact part's text.
func sendResult(endpoint string, payload map[string]any) map[string]any {
	result, _ := payload["result"].(map[string]any)
	task := result
	if nested, ok := result["task"].(map[string]any); ok {
		task = nested
	}
	texts := []string{}
	for _, artifact := range listOf(task["artifacts"]) {
		entry, _ := artifact.(map[string]any)
		for _, part := range listOf(entry["parts"]) {
			partMap, _ := part.(map[string]any)
			if text, _ := partMap["text"].(string); text != "" {
				texts = append(texts, text)
			}
		}
	}
	var state any
	if status, ok := task["status"].(map[string]any); ok {
		state = status["state"]
	}
	return map[string]any{
		"http_status": http.StatusOK,
		"agent":       endpoint,
		"task_id":     task["id"],
		"context_id":  task["contextId"],
		"state":       state,
		"reply":       strings.Join(texts, "\n"),
	}
}

func (c *client) card(ctx context.Context) (map[string]any, error) {
	//nolint:gosec // G704: baseURL is operator configuration, not caller input
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+AgentCardPath, nil)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "build agent card request failed")
	}
	//nolint:gosec // G704: baseURL is operator configuration, not caller input
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "fetch agent card from %s failed", c.baseURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf(
			ctx,
			"fetch agent card from %s failed: HTTP %d",
			c.baseURL,
			resp.StatusCode,
		)
	}
	var card map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&card); err != nil {
		return nil, errors.Wrapf(ctx, err, "decode agent card failed")
	}
	return card, nil
}

// interfaceURL returns supportedInterfaces[0].url, or nil when the card advertises none.
func interfaceURL(card map[string]any) any {
	interfaces := listOf(card["supportedInterfaces"])
	if len(interfaces) == 0 {
		return nil
	}
	first, _ := interfaces[0].(map[string]any)
	return first["url"]
}

func listOf(value any) []any {
	list, _ := value.([]any)
	return list
}
