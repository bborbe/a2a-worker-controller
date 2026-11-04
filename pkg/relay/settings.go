// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package relay

import (
	"context"
	"strings"

	"github.com/bborbe/errors"
)

// ResolveSettings validates the relay's two deployment settings together and returns them
// trimmed.
//
// The relay is optional, and its two settings are required together: a listener with no secret
// would serve a pull endpoint that anyone who can reach it could drain. Refused at startup,
// rather than left to fail on the first pull.
//
// ⚠️ THE SECRET IS TRIMMED, NOT MERELY CHECKED FOR EMPTINESS. `RELAY_SECRET=" "` is a secret of
// one space: non-empty, so an emptiness check accepts it, and trivially guessable. Trimming
// makes a misconfigured environment stop at startup instead of serving a credential anyone can
// present. A whitespace-only listen is likewise trimmed to empty, which disables the relay —
// the same reading the setting's own usage gives "empty".
//
// It lives here rather than in the caller so the guard is covered by this package's suite: the
// controller's own test file builds the binary and exercises no flag combination at all, so a
// regression in the guard would ship silently — and the guard is what stands between a listener
// and an unauthenticated pull endpoint.
func ResolveSettings(ctx context.Context, listen string, secret string) (string, string, error) {
	listen = strings.TrimSpace(listen)
	secret = strings.TrimSpace(secret)
	if listen == "" {
		return "", "", nil
	}
	if secret == "" {
		return "", "", errors.Errorf(ctx, "RELAY_LISTEN requires RELAY_SECRET")
	}
	return listen, secret, nil
}
