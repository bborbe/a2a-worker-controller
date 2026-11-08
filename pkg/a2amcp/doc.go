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
