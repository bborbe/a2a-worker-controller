// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg/a2amcp"
)

var _ = Describe("LoadOrCreateBearer", func() {
	var (
		ctx  context.Context
		path string
	)

	BeforeEach(func() {
		ctx = context.Background()
		path = filepath.Join(GinkgoT().TempDir(), "state", a2amcp.BearerFileName)
	})

	It("creates a 0600 bearer of 64 hex characters on first use", func() {
		bearer, err := a2amcp.LoadOrCreateBearer(ctx, path)
		Expect(err).NotTo(HaveOccurred())
		Expect(bearer).To(MatchRegexp(`^[0-9a-f]{64}$`))
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		dirInfo, err := os.Stat(filepath.Dir(path))
		Expect(err).NotTo(HaveOccurred())
		Expect(dirInfo.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	It("returns the same bearer on every later call", func() {
		first, err := a2amcp.LoadOrCreateBearer(ctx, path)
		Expect(err).NotTo(HaveOccurred())
		second, err := a2amcp.LoadOrCreateBearer(ctx, path)
		Expect(err).NotTo(HaveOccurred())
		Expect(second).To(Equal(first))
	})

	It("refuses a bearer file readable by group or others", func() {
		Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
		Expect(os.WriteFile(path, []byte("abc\n"), 0o644)).To(Succeed())
		_, err := a2amcp.LoadOrCreateBearer(ctx, path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("readable by group or others"))
		Expect(err.Error()).NotTo(ContainSubstring("abc"))
	})

	It("refuses an empty bearer file", func() {
		Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
		Expect(os.WriteFile(path, []byte("\n"), 0o600)).To(Succeed())
		_, err := a2amcp.LoadOrCreateBearer(ctx, path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("is empty"))
	})
})
