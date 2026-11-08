// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg/a2amcp"
)

var _ = Describe("TokenSource", func() {
	var (
		ctx       context.Context
		keychain  *mocks.A2amcpKeychainReader
		teamVault *mocks.PkgTeamVaultResolver
		config    a2amcp.TokenConfig
	)

	BeforeEach(func() {
		ctx = context.Background()
		keychain = &mocks.A2amcpKeychainReader{}
		teamVault = &mocks.PkgTeamVaultResolver{}
		config = a2amcp.TokenConfig{
			TargetURL:       "http://localhost:9091",
			KeychainService: "agent-claude-local",
			KeychainAccount: "a2a-token",
			TeamVaultKey:    "key1",
		}
		keychain.ReadReturns("from-keychain", nil)
		teamVault.ResolveReturns("from-teamvault", nil)
	})

	token := func() (string, error) {
		return a2amcp.NewTokenSource(config, keychain, teamVault).Token(ctx)
	}

	It("prefers the explicit token over every other branch", func() {
		config.Token = "explicit"
		Expect(token()).To(Equal("explicit"))
		Expect(keychain.ReadCallCount()).To(Equal(0))
		Expect(teamVault.ResolveCallCount()).To(Equal(0))
	})

	It("reads the Keychain item for a localhost target", func() {
		Expect(token()).To(Equal("from-keychain"))
		_, service, account := keychain.ReadArgsForCall(0)
		Expect(service).To(Equal("agent-claude-local"))
		Expect(account).To(Equal("a2a-token"))
		Expect(teamVault.ResolveCallCount()).To(Equal(0))
	})

	It("reads the Keychain item for a 127.0.0.1 target", func() {
		config.TargetURL = "http://127.0.0.1:9091"
		Expect(token()).To(Equal("from-keychain"))
	})

	It("skips the Keychain for a remote target and uses TeamVault", func() {
		config.TargetURL = "https://claude-interactive.dev.example"
		Expect(token()).To(Equal("from-teamvault"))
		Expect(keychain.ReadCallCount()).To(Equal(0))
		_, key := teamVault.ResolveArgsForCall(0)
		Expect(key).To(Equal("key1"))
	})

	It("falls through to TeamVault when the Keychain lookup fails", func() {
		keychain.ReadReturns("", errors.New("item not found"))
		Expect(token()).To(Equal("from-teamvault"))
	})

	It("fails when every branch yields nothing", func() {
		keychain.ReadReturns("", errors.New("item not found"))
		teamVault.ResolveReturns("", errors.New("lookup failed"))
		_, err := token()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to resolve A2A token"))
	})

	It("fails without consulting TeamVault when no key is configured", func() {
		keychain.ReadReturns("", errors.New("item not found"))
		config.TeamVaultKey = ""
		_, err := token()
		Expect(err).To(HaveOccurred())
		Expect(teamVault.ResolveCallCount()).To(Equal(0))
	})
})
