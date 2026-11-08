// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package factory

import (
	"net/http"

	"github.com/bborbe/a2a-worker-controller/pkg"
	"github.com/bborbe/a2a-worker-controller/pkg/a2amcp"
	"github.com/bborbe/a2a-worker-controller/pkg/handler"
)

// CreateA2AMCPHandler wires the /mcp a2a tools: an A2A client for targetURL whose token
// follows tokenConfig's precedence (explicit, else Keychain for a local target, else
// TeamVault under teamVaultConfig), behind the local bearer.
func CreateA2AMCPHandler(
	tokenConfig a2amcp.TokenConfig,
	teamVaultConfig string,
	bearer string,
) http.Handler {
	tokens := a2amcp.NewTokenSource(
		tokenConfig,
		a2amcp.NewKeychainReader(),
		pkg.NewTeamVaultResolverWithConfig(teamVaultConfig),
	)
	return a2amcp.NewHandler(
		a2amcp.NewClient(
			&http.Client{Timeout: a2amcp.DefaultTimeout},
			tokenConfig.TargetURL,
			tokens,
		),
		bearer,
	)
}

// CreateHealthzHandler creates an HTTP handler that serves the canonical
// `/healthz` liveness response (HTTP 200, body `{"status":"ok"}`,
// Content-Type: application/json).
func CreateHealthzHandler() http.Handler {
	return handler.NewHealthzHandler()
}
