// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

var _ = Describe("ParseMountAllowlist", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("parses a comma-separated absolute path list", func() {
		allowlist, err := pkg.ParseMountAllowlist(ctx, "/srv/data, /srv/other")
		Expect(err).To(BeNil())
		Expect(allowlist).To(Equal(pkg.MountAllowlist{"/srv/data", "/srv/other"}))
	})

	It("reads an empty value as an allowlist that permits nothing", func() {
		allowlist, err := pkg.ParseMountAllowlist(ctx, "")
		Expect(err).To(BeNil())
		Expect(allowlist).To(BeEmpty())
		// The empty allowlist is the load-bearing half of this case: it must refuse a path,
		// not fall through to allowing it.
		Expect(allowlist.Allows("/srv/data")).To(BeFalse())
	})

	It("cleans a path so two spellings of one directory compare equal", func() {
		allowlist, err := pkg.ParseMountAllowlist(ctx, "/srv/./data/")
		Expect(err).To(BeNil())
		Expect(allowlist).To(Equal(pkg.MountAllowlist{"/srv/data"}))
	})

	DescribeTable("refuses a malformed entry rather than dropping it",
		func(value string) {
			_, err := pkg.ParseMountAllowlist(ctx, value)
			Expect(err).NotTo(BeNil())
		},
		Entry("relative path", "srv/data"),
		Entry("bare name", "data"),
		Entry("relative among absolute", "/srv/data,other"),
	)
})

var _ = Describe("MountAllowlist.Allows", func() {
	allowlist := pkg.MountAllowlist{"/srv/data"}

	It("admits the allowlisted path itself", func() {
		Expect(allowlist.Allows("/srv/data")).To(BeTrue())
	})

	It("admits a path beneath it", func() {
		Expect(allowlist.Allows("/srv/data/vault")).To(BeTrue())
	})

	// The whole reason the comparison is on path boundaries: a raw prefix match would admit a
	// DIFFERENT directory the operator never named.
	It("refuses a sibling whose name merely starts with the same characters", func() {
		Expect(allowlist.Allows("/srv/database")).To(BeFalse())
	})

	It("refuses a parent of the allowlisted path", func() {
		Expect(allowlist.Allows("/srv")).To(BeFalse())
	})

	It("refuses an unrelated path", func() {
		Expect(allowlist.Allows("/etc")).To(BeFalse())
	})

	It("admits a descendant spelled with a redundant separator", func() {
		Expect(allowlist.Allows("/srv/data//vault")).To(BeTrue())
	})
})

var _ = Describe("ValidateMounts", func() {
	var ctx context.Context
	allowlist := pkg.MountAllowlist{"/srv/data"}

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("accepts an allowlisted mount", func() {
		err := pkg.ValidateMounts(ctx, allowlist, []pkg.Mount{
			{HostPath: "/srv/data", ContainerPath: "/vault"},
		})
		Expect(err).To(BeNil())
	})

	It("refuses a host path outside the allowlist, naming it", func() {
		err := pkg.ValidateMounts(ctx, allowlist, []pkg.Mount{
			{HostPath: "/etc", ContainerPath: "/etc"},
		})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring(pkg.MountNotAllowedPrefix))
		Expect(err.Error()).To(ContainSubstring("/etc"))
	})

	// A named volume is not a host path, so the allowlist cannot govern it — refusing it by
	// name is what keeps it from being an ungoverned mount surface.
	It("refuses a named volume, naming the volume", func() {
		err := pkg.ValidateMounts(ctx, allowlist, []pkg.Mount{
			{Volume: "some-other-workload", ContainerPath: "/data"},
		})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring(pkg.MountNotAllowedPrefix))
		Expect(err.Error()).To(ContainSubstring("volume"))
		Expect(err.Error()).To(ContainSubstring("some-other-workload"))
	})

	It("refuses a mount declaring neither host path nor volume", func() {
		err := pkg.ValidateMounts(ctx, allowlist, []pkg.Mount{{ContainerPath: "/vault"}})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring(pkg.MountNotAllowedPrefix))
	})

	It("refuses a relative host path even when it would fall under an entry", func() {
		err := pkg.ValidateMounts(ctx, pkg.MountAllowlist{"srv/data"}, []pkg.Mount{
			{HostPath: "srv/data", ContainerPath: "/vault"},
		})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring(pkg.MountNotAllowedPrefix))
	})

	It("refuses a mount with no container path", func() {
		err := pkg.ValidateMounts(ctx, allowlist, []pkg.Mount{{HostPath: "/srv/data"}})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring(pkg.MountNotAllowedPrefix))
	})

	It("accepts an empty mount list", func() {
		Expect(pkg.ValidateMounts(ctx, allowlist, nil)).To(BeNil())
	})
})

var _ = Describe("ValidateSecrets", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("accepts a well-formed secret", func() {
		err := pkg.ValidateSecrets(ctx, []pkg.Secret{
			{Name: "GATEWAY_SECRET", TeamVaultKey: "9L6eMO"},
		})
		Expect(err).To(BeNil())
	})

	It("refuses a secret with no name", func() {
		err := pkg.ValidateSecrets(ctx, []pkg.Secret{{TeamVaultKey: "9L6eMO"}})
		Expect(err).NotTo(BeNil())
	})

	It("refuses a secret with no teamvault key, naming the secret", func() {
		err := pkg.ValidateSecrets(ctx, []pkg.Secret{{Name: "GATEWAY_SECRET"}})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("GATEWAY_SECRET"))
	})

	// Two secrets under one name would make the worker's view of that name depend on the
	// controller's iteration order, which a caller cannot reason about.
	It("refuses a name declared twice", func() {
		err := pkg.ValidateSecrets(ctx, []pkg.Secret{
			{Name: "TOKEN", TeamVaultKey: "aaaaaa"},
			{Name: "TOKEN", TeamVaultKey: "bbbbbb"},
		})
		Expect(err).NotTo(BeNil())
		Expect(err.Error()).To(ContainSubstring("TOKEN"))
	})

	It("accepts an empty secret list", func() {
		Expect(pkg.ValidateSecrets(ctx, nil)).To(BeNil())
	})
})
