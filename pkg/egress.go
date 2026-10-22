// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/bborbe/errors"
)

// cidrPrefix marks an external destination in the EGRESS_ALLOW list, e.g.
// `cidr=140.82.112.0/20:443`. It is a prefix rather than a bare `a.b.c.d/n:port` form because
// the existing entry already reads `target/selector:port`, so a CIDR written bare would be
// ambiguous with a namespace and a pod label.
const cidrPrefix = "cidr="

// EgressDestination is one destination a worker pod may reach on TCP Port. Exactly one of
// Namespace or IPBlock is set.
//
// Namespace names an in-cluster destination: the pods in it — all of them, or only those
// matching PodLabels — on TCP Port. DNS is not one of them — every worker policy allows it
// implicitly, because without it no other destination resolves.
//
// PodLabels exists because a namespace-wide port is too wide on a shared namespace: in nuke's
// `dev`, port 9090 is served by the vault writers and by the trading gateways alike, so
// `dev:9090` would open both. `dev/app=vault-obsidian-personal:9090` opens the vault alone.
//
// IPBlock names an EXTERNAL destination by CIDR, and exists because a worker's task may need
// the public internet: `make check` clones from github.com, fetches modules from
// proxy.golang.org and pulls trivy's database from ghcr.io, and a namespace peer cannot express
// any of them. ⚠️ A NetworkPolicy matches addresses and never names, so a provider's published
// ranges must be listed rather than its hostname — and a provider that moves its addresses
// needs this list updated, which no DNS lookup can do for it.
type EgressDestination struct {
	Namespace string
	PodLabels map[string]string
	IPBlock   string
	Port      int32
}

// ParseEgressDestinations parses a comma-separated list of `namespace[/key=value]:port` and
// `cidr=<CIDR>:port` entries, the form the EGRESS_ALLOW setting takes
// (`dev:8788,dev/app=vault-obsidian-personal:9090,cidr=140.82.112.0/20:443`). The optional
// `/key=value` narrows an in-cluster destination to pods carrying that label. An empty value is
// an empty list — a worker that may reach nothing but DNS. A malformed entry is refused rather
// than skipped, because a dropped entry silently denies a destination the operator meant to
// allow.
func ParseEgressDestinations(ctx context.Context, value string) ([]EgressDestination, error) {
	var destinations []EgressDestination
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if rest, isCIDR := strings.CutPrefix(entry, cidrPrefix); isCIDR {
			destination, err := parseCIDRDestination(ctx, entry, rest)
			if err != nil {
				return nil, err
			}
			destinations = append(destinations, destination)
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
		port, err := parseEgressPort(ctx, entry, portValue)
		if err != nil {
			return nil, err
		}
		destinations = append(
			destinations,
			EgressDestination{Namespace: namespace, PodLabels: podLabels, Port: port},
		)
	}
	return destinations, nil
}

// parseCIDRDestination parses the part of an external entry after its `cidr=` prefix. The CIDR
// is normalised to its network address, so `140.82.112.5/20` and `140.82.112.0/20` produce the
// same policy rule rather than two spellings of one destination.
func parseCIDRDestination(
	ctx context.Context,
	entry string,
	rest string,
) (EgressDestination, error) {
	cidrValue, portValue, found := strings.Cut(rest, ":")
	if !found || cidrValue == "" {
		return EgressDestination{}, errors.Errorf(
			ctx,
			"egress entry %q is not cidr=<CIDR>:port",
			entry,
		)
	}
	_, network, err := net.ParseCIDR(cidrValue)
	if err != nil {
		return EgressDestination{}, errors.Errorf(ctx, "egress entry %q has no valid CIDR", entry)
	}
	port, err := parseEgressPort(ctx, entry, portValue)
	if err != nil {
		return EgressDestination{}, err
	}
	return EgressDestination{IPBlock: network.String(), Port: port}, nil
}

// parseEgressPort reads the trailing port of an entry, shared by both destination forms so one
// range check governs them.
func parseEgressPort(ctx context.Context, entry string, portValue string) (int32, error) {
	port, err := strconv.ParseInt(portValue, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.Errorf(ctx, "egress entry %q has no valid port", entry)
	}
	return int32(port), nil
}
