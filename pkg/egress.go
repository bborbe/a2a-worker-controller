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

// EgressDestination is one in-cluster destination a worker pod may reach: any pod in
// Namespace, on TCP Port. DNS is not one of them — every worker policy allows it implicitly,
// because without it no other destination resolves.
type EgressDestination struct {
	Namespace string
	Port      int32
}

// ParseEgressDestinations parses a comma-separated `namespace:port` list, the form the
// EGRESS_ALLOW setting takes (`dev:8788,monitoring:9090`). An empty value is an empty list —
// a worker that may reach nothing but DNS. A malformed entry is refused rather than skipped,
// because a dropped entry silently denies a destination the operator meant to allow.
func ParseEgressDestinations(ctx context.Context, value string) ([]EgressDestination, error) {
	var destinations []EgressDestination
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		namespace, portValue, found := strings.Cut(entry, ":")
		if !found || namespace == "" {
			return nil, errors.Errorf(ctx, "egress entry %q is not namespace:port", entry)
		}
		port, err := strconv.ParseInt(portValue, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.Errorf(ctx, "egress entry %q has no valid port", entry)
		}
		destinations = append(
			destinations,
			EgressDestination{Namespace: namespace, Port: int32(port)},
		)
	}
	return destinations, nil
}
