// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"

	"github.com/bborbe/errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// stubResolver is a TeamVaultResolver that answers from a map, so the resolution path can be
// exercised without a TeamVault instance.
type stubResolver struct {
	values map[string]string
	err    error
	asked  []string
}

func (s *stubResolver) Resolve(_ context.Context, key string) (string, error) {
	s.asked = append(s.asked, key)
	if s.err != nil {
		return "", s.err
	}
	return s.values[key], nil
}

var _ = Describe("resolveSecrets", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("resolves each declared key and pairs it with its name, in order", func() {
		resolver := &stubResolver{values: map[string]string{"aaaaaa": "one", "bbbbbb": "two"}}
		resolved, err := resolveSecrets(ctx, resolver, []Secret{
			{Name: "FIRST", TeamVaultKey: "aaaaaa"},
			{Name: "SECOND", TeamVaultKey: "bbbbbb"},
		})
		Expect(err).To(BeNil())
		Expect(resolved).To(Equal([]ResolvedSecret{
			{Name: "FIRST", Value: "one"},
			{Name: "SECOND", Value: "two"},
		}))
		Expect(resolver.asked).To(Equal([]string{"aaaaaa", "bbbbbb"}))
	})

	It("resolves nothing when no secret is declared", func() {
		resolved, err := resolveSecrets(ctx, nil, nil)
		Expect(err).To(BeNil())
		Expect(resolved).To(BeEmpty())
	})

	// A worker missing a credential it was told to expect starts successfully and fails at its
	// first authenticated call — far from the cause, on a surface that already reported success.
	It("refuses when a secret is declared and no resolver is configured", func() {
		_, err := resolveSecrets(ctx, nil, []Secret{{Name: "TOKEN", TeamVaultKey: "aaaaaa"}})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("TeamVault"))
	})

	It("propagates a resolver failure", func() {
		resolver := &stubResolver{err: errors.Errorf(ctx, "lookup failed for key aaaaaa")}
		_, err := resolveSecrets(ctx, resolver, []Secret{{Name: "TOKEN", TeamVaultKey: "aaaaaa"}})
		Expect(err).NotTo(BeNil())
	})
})

var _ = Describe("teamVaultCLI.Resolve", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("builds the teamvault-cli password invocation and returns its stdout", func() {
		var argv []string
		resolver := &teamVaultCLI{
			command: "teamvault-cli",
			config:  "/tmp/teamvault.json",
			run: func(_ context.Context, a []string) (string, string, error) {
				argv = a
				return "the-value\n", "", nil
			},
		}
		value, err := resolver.Resolve(ctx, "aaaaaa")
		Expect(err).To(BeNil())
		Expect(value).To(Equal("the-value"))
		Expect(argv).To(Equal([]string{
			"teamvault-cli", "password",
			"--teamvault-key=aaaaaa",
			"--teamvault-config=/tmp/teamvault.json",
		}))
	})

	// The config flag is omitted rather than defaulted, so teamvault-cli's own precedence is the
	// only one in the system.
	It("omits the config flag when none is configured", func() {
		var argv []string
		resolver := &teamVaultCLI{
			command: "teamvault-cli",
			run: func(_ context.Context, a []string) (string, string, error) {
				argv = a
				return "the-value", "", nil
			},
		}
		_, err := resolver.Resolve(ctx, "aaaaaa")
		Expect(err).To(BeNil())
		Expect(argv).To(Equal([]string{"teamvault-cli", "password", "--teamvault-key=aaaaaa"}))
	})

	It("refuses an empty key without running anything", func() {
		ran := false
		resolver := &teamVaultCLI{
			command: "teamvault-cli",
			run: func(_ context.Context, _ []string) (string, string, error) {
				ran = true
				return "", "", nil
			},
		}
		_, err := resolver.Resolve(ctx, "")
		Expect(err).NotTo(BeNil())
		Expect(ran).To(BeFalse())
	})

	It("refuses an empty value", func() {
		resolver := &teamVaultCLI{
			command: "teamvault-cli",
			run: func(_ context.Context, _ []string) (string, string, error) {
				return "   \n", "", nil
			},
		}
		_, err := resolver.Resolve(ctx, "aaaaaa")
		Expect(err).NotTo(BeNil())
	})

	// The error names the KEY and the CLI's stderr. It must never carry stdout, which is the
	// secret.
	It("names the key and the CLI stderr on failure, never the value", func() {
		resolver := &teamVaultCLI{
			command: "teamvault-cli",
			run: func(_ context.Context, _ []string) (string, string, error) {
				return "leaked-value", "no such key", errors.Errorf(ctx, "exit 1")
			},
		}
		_, err := resolver.Resolve(ctx, "aaaaaa")
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("aaaaaa"))
		Expect(err.Error()).To(ContainSubstring("no such key"))
		Expect(err.Error()).NotTo(ContainSubstring("leaked-value"))
	})
})
