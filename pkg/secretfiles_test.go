// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

var _ = Describe("WriteSecretFiles", func() {
	var ctx context.Context
	var dir string
	const workerID = pkg.WorkerID("aaaaaaaabbbbbbbb")

	BeforeEach(func() {
		ctx = context.Background()
		dir = GinkgoT().TempDir()
	})

	It("writes one file per secret and returns the mounts in order", func() {
		mounts, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
			{Name: "OTHER", Value: "canary-two"},
		})
		Expect(err).To(BeNil())
		Expect(mounts).To(HaveLen(2))
		Expect(mounts[0].Name).To(Equal("GATEWAY_SECRET"))
		Expect(mounts[1].Name).To(Equal("OTHER"))
	})

	It("writes the value to the returned host path", func() {
		mounts, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
		})
		Expect(err).To(BeNil())
		content, err := os.ReadFile(mounts[0].HostPath)
		Expect(err).To(BeNil())
		Expect(string(content)).To(Equal("canary-one"))
	})

	// The mode is the whole reason this is a file rather than an env var: a 0644 secret is
	// readable by every local process, which is the exposure the delivery route exists to avoid.
	It("writes the file 0600", func() {
		mounts, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
		})
		Expect(err).To(BeNil())
		info, err := os.Stat(mounts[0].HostPath)
		Expect(err).To(BeNil())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("writes the worker directory 0700", func() {
		_, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
		})
		Expect(err).To(BeNil())
		info, err := os.Stat(filepath.Join(dir, string(workerID)))
		Expect(err).To(BeNil())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	It("returns nothing for an empty secret list", func() {
		mounts, err := pkg.WriteSecretFiles(ctx, dir, workerID, nil)
		Expect(err).To(BeNil())
		Expect(mounts).To(BeEmpty())
	})

	It("refuses when no secret directory is configured", func() {
		_, err := pkg.WriteSecretFiles(ctx, "", workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
		})
		Expect(err).NotTo(BeNil())
	})

	// O_EXCL: overwriting would replace a credential another worker is still reading.
	It("refuses to overwrite an existing secret file", func() {
		_, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "first"},
		})
		Expect(err).To(BeNil())
		_, err = pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "second"},
		})
		Expect(err).NotTo(BeNil())
	})
})

var _ = Describe("RemoveSecretFiles", func() {
	var ctx context.Context
	var dir string
	const workerID = pkg.WorkerID("aaaaaaaabbbbbbbb")

	BeforeEach(func() {
		ctx = context.Background()
		dir = GinkgoT().TempDir()
	})

	It("removes the worker's secret directory", func() {
		_, err := pkg.WriteSecretFiles(ctx, dir, workerID, []pkg.ResolvedSecret{
			{Name: "GATEWAY_SECRET", Value: "canary-one"},
		})
		Expect(err).To(BeNil())
		Expect(pkg.RemoveSecretFiles(ctx, dir, workerID)).To(BeNil())
		_, err = os.Stat(filepath.Join(dir, string(workerID)))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	// A worker created without secrets has no directory, and the stop that follows must not
	// fail on that account.
	It("is not an error when there is nothing to remove", func() {
		Expect(pkg.RemoveSecretFiles(ctx, dir, workerID)).To(BeNil())
	})

	It("is not an error when no secret directory is configured", func() {
		Expect(pkg.RemoveSecretFiles(ctx, "", workerID)).To(BeNil())
	})
})
