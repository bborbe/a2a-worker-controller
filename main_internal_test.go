// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"testing"
)

// relaySettingsCase is one row of the startup-guard table.
type relaySettingsCase struct {
	name        string
	listen      string
	secret      string
	wantListen  string
	wantSecret  string
	wantErrText string
}

// TestResolveRelaySettings covers the startup guard on its own, which the build-only suite in
// main_test.go cannot: it exercises no flag combination at all, so a regression in this guard
// would ship silently — and the guard is what stands between a listener and an unauthenticated
// pull endpoint.
func TestResolveRelaySettings(t *testing.T) {
	tests := []relaySettingsCase{
		{
			name:       "relay disabled",
			listen:     "",
			secret:     "",
			wantListen: "",
			wantSecret: "",
		},
		{
			// A secret with no listener is not an error: the secret is inert until the relay
			// is switched on, and refusing here would make a stage that never runs a relay
			// unable to carry the key at all.
			name:       "secret without listener is inert",
			listen:     "",
			secret:     "s3cret",
			wantListen: "",
			wantSecret: "",
		},
		{
			name:       "relay enabled with secret",
			listen:     ":9091",
			secret:     "s3cret",
			wantListen: ":9091",
			wantSecret: "s3cret",
		},
		{
			name:        "relay enabled without secret is refused",
			listen:      ":9091",
			secret:      "",
			wantErrText: "RELAY_LISTEN requires RELAY_SECRET",
		},
		{
			// The one this guard exists for: `RELAY_SECRET=" "` is non-empty, so a bare
			// emptiness check accepts it, and it is a credential anyone can guess.
			name:        "all-space secret is refused",
			listen:      ":9091",
			secret:      " ",
			wantErrText: "RELAY_LISTEN requires RELAY_SECRET",
		},
		{
			// A whitespace-only listener trims to empty, which disables the relay — the same
			// reading the flag's own usage gives "empty".
			name:       "all-space listener disables the relay",
			listen:     " ",
			secret:     "s3cret",
			wantListen: "",
			wantSecret: "",
		},
		{
			name:       "surrounding whitespace is trimmed from both",
			listen:     " :9091 ",
			secret:     " s3cret ",
			wantListen: ":9091",
			wantSecret: "s3cret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRelaySettings(t, test)
		})
	}
}

// assertRelaySettings runs one table row against the guard and reports the first divergence.
func assertRelaySettings(t *testing.T, test relaySettingsCase) {
	t.Helper()

	listen, secret, err := resolveRelaySettings(context.Background(), test.listen, test.secret)

	if test.wantErrText != "" {
		if err == nil {
			t.Fatalf("expected error %q, got none", test.wantErrText)
		}
		if err.Error() != test.wantErrText {
			t.Fatalf("error = %q, want %q", err.Error(), test.wantErrText)
		}
		return
	}

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listen != test.wantListen {
		t.Errorf("listen = %q, want %q", listen, test.wantListen)
	}
	if secret != test.wantSecret {
		t.Errorf("secret = %q, want %q", secret, test.wantSecret)
	}
}
