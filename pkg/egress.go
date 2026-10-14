// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"strconv"
	"strings"

	"github.com/bborbe/errors"
)

// EgressDestination is one in-cluster destination a worker pod may reach: the pods in
// Namespace — all of them, or only those matching PodLabels — on TCP Port. DNS is not one of
// them — every worker policy allows it implicitly, because without it no other destination
// resolves.
//
// PodLabels exists because a namespace-wide port is too wide on a shared namespace: in nuke's
// `dev`, port 9090 is served by the vault writers and by the trading gateways alike, so
// `dev:9090` would open both. `dev/app=vault-obsidian-personal:9090` opens the vault alone.
type EgressDestination struct {
	Namespace string
	PodLabels map[string]string
	Port      int32
}

// ParseEgressDestinations parses a comma-separated `namespace[/key=value]:port` list, the form
// the EGRESS_ALLOW setting takes (`dev:8788,dev/app=vault-obsidian-personal:9090`). The
// optional `/key=value` narrows the destination to pods carrying that label. An empty value is
// an empty list —
// a worker that may reach nothing but DNS. A malformed entry is refused rather than skipped,
// because a dropped entry silently denies a destination the operator meant to allow.
func ParseEgressDestinations(ctx context.Context, value string) ([]EgressDestination, error) {
	var destinations []EgressDestination
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		target, portValue, found := strings.Cut(entry, ":")
		namespace, selector, hasSelector := strings.Cut(target, "/")
		if !found || namespace == "" {
			return nil, errors.Errorf(
				ctx,
				"egress entry %q is not namespace[/key=value]:port",
				entry,
			)
		}
		var podLabels map[string]string
		if hasSelector {
			key, value, ok := strings.Cut(selector, "=")
			if !ok || key == "" || value == "" {
				return nil, errors.Errorf(
					ctx,
					"egress entry %q has no valid key=value pod label",
					entry,
				)
			}
			podLabels = map[string]string{key: value}
		}
		port, err := strconv.ParseInt(portValue, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.Errorf(ctx, "egress entry %q has no valid port", entry)
		}
		destinations = append(
			destinations,
			EgressDestination{Namespace: namespace, PodLabels: podLabels, Port: int32(port)},
		)
	}
	return destinations, nil
}
