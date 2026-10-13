// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// A2APath is the route the A2A JSON-RPC binding is served at.
const A2APath = "/a2a"

// AgentCardPath is the well-known path the Agent Card is served at, as the A2A spec defines
// it. It is the SDK's own constant, re-exported so the service's wiring reads both A2A
// routes from one place.
const AgentCardPath = a2asrv.WellKnownAgentCardPath

// maxRequestBodyBytes caps the A2A request body before the SDK handler decodes it. The SDK
// decodes with json.NewDecoder and applies no limit of its own, so without this cap a
// caller could exhaust the pod's memory with an unbounded body. A truncated JSON body
// cannot parse, so an oversized request is refused rather than processed.
const maxRequestBodyBytes = 1 << 20

// agentCardVersion is the Agent Card's own version string. The format is the provider's to
// choose; this service has no separate release version to borrow.
const agentCardVersion = "1.0.0"

// AgentCardConfig is what the A2A Agent Card advertises about this service.
type AgentCardConfig struct {
	// Name names the deployed agent in the Agent Card.
	Name string
	// PublicURL is the externally reachable A2A endpoint the card advertises verbatim.
	// It is never derived from the listen address or a request header.
	PublicURL string
}

// NewAgentCard builds the Agent Card this service advertises. Its single supported
// interface is the JSON-RPC binding at config.PublicURL, which is the externally reachable
// address supplied as configuration — never the listen address and never a value derived
// from a request. The card carries no credential.
func NewAgentCard(config AgentCardConfig) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        config.Name,
		Version:     agentCardVersion,
		Description: "Creates, lists and stops isolated workers for vault tasks, over A2A.",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(config.PublicURL, a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"application/json"},
		DefaultOutputModes: []string{"application/json"},
		Skills: []a2a.AgentSkill{
			{
				ID:          "control",
				Name:        "Control",
				Description: "Creates, lists and stops workers for vault tasks.",
				Tags:        []string{"control"},
			},
		},
	}
}

// NewAgentCardHandler serves the Agent Card. The card is public by design: it advertises
// the endpoint and skills and carries no credential.
func NewAgentCardHandler(card *a2a.AgentCard) http.Handler {
	return a2asrv.NewStaticAgentCardHandler(card)
}

// NewA2AHandler serves the A2A JSON-RPC binding over the given executor. The body is capped
// before the SDK handler sees it.
func NewA2AHandler(executor a2asrv.AgentExecutor) http.Handler {
	inner := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor))
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(resp, req.Body, maxRequestBodyBytes)
		inner.ServeHTTP(resp, req)
	})
}
