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

var _ = Describe("JSONTokenStore", func() {
	var (
		ctx  context.Context
		path string
	)

	BeforeEach(func() {
		ctx = context.Background()
		// A subdirectory, not the temp dir itself: the store creates its directory with 0750
		// and a TempDir is already 0700, which would hide that.
		path = filepath.Join(GinkgoT().TempDir(), "state", "docker-workers.json")
	})

	It("reports an absent entry as empty rather than as an error", func() {
		store := pkg.NewJSONTokenStore(path)

		token, err := store.Get(ctx, pkg.WorkerID("missing"))

		Expect(err).To(BeNil())
		Expect(token).To(BeEmpty())
	})

	It("returns a token it stored", func() {
		store := pkg.NewJSONTokenStore(path)

		Expect(store.Put(ctx, pkg.WorkerID("w1"), "token-one")).To(BeNil())

		token, err := store.Get(ctx, pkg.WorkerID("w1"))
		Expect(err).To(BeNil())
		Expect(token).To(Equal("token-one"))
	})

	It("replaces a worker's token rather than adding a second entry", func() {
		store := pkg.NewJSONTokenStore(path)
		Expect(store.Put(ctx, pkg.WorkerID("w1"), "first")).To(BeNil())

		Expect(store.Put(ctx, pkg.WorkerID("w1"), "second")).To(BeNil())

		token, err := store.Get(ctx, pkg.WorkerID("w1"))
		Expect(err).To(BeNil())
		Expect(token).To(Equal("second"))
	})

	It("keeps entries for different workers apart", func() {
		store := pkg.NewJSONTokenStore(path)
		Expect(store.Put(ctx, pkg.WorkerID("w1"), "one")).To(BeNil())
		Expect(store.Put(ctx, pkg.WorkerID("w2"), "two")).To(BeNil())

		one, err := store.Get(ctx, pkg.WorkerID("w1"))
		Expect(err).To(BeNil())
		two, err := store.Get(ctx, pkg.WorkerID("w2"))
		Expect(err).To(BeNil())
		Expect(one).To(Equal("one"))
		Expect(two).To(Equal("two"))
	})

	It("removes a worker's entry on delete", func() {
		store := pkg.NewJSONTokenStore(path)
		Expect(store.Put(ctx, pkg.WorkerID("w1"), "one")).To(BeNil())

		Expect(store.Delete(ctx, pkg.WorkerID("w1"))).To(BeNil())

		token, err := store.Get(ctx, pkg.WorkerID("w1"))
		Expect(err).To(BeNil())
		Expect(token).To(BeEmpty())
	})

	It("treats a delete of an absent entry as a no-op", func() {
		store := pkg.NewJSONTokenStore(path)

		Expect(store.Delete(ctx, pkg.WorkerID("missing"))).To(BeNil())
	})

	// ⚠️ THIS IS THE SC2 PROPERTY. The token is at rest so a controller restart — a new store
	// instance over the same file — can still hand it back.
	It("survives a new store instance over the same file", func() {
		Expect(pkg.NewJSONTokenStore(path).Put(ctx, pkg.WorkerID("w1"), "kept")).To(BeNil())

		token, err := pkg.NewJSONTokenStore(path).Get(ctx, pkg.WorkerID("w1"))

		Expect(err).To(BeNil())
		Expect(token).To(Equal("kept"))
	})

	// ⚠️ THE FILE HOLDS A CREDENTIAL. 0600 is the assertion that matters; the directory is
	// created 0750 so no other user can even list it.
	It("writes the store 0600 in a 0750 directory", func() {
		store := pkg.NewJSONTokenStore(path)

		Expect(store.Put(ctx, pkg.WorkerID("w1"), "secret")).To(BeNil())

		fileInfo, err := os.Stat(path)
		Expect(err).To(BeNil())
		Expect(fileInfo.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		dirInfo, err := os.Stat(filepath.Dir(path))
		Expect(err).To(BeNil())
		Expect(dirInfo.Mode().Perm()).To(Equal(os.FileMode(0o750)))
	})

	It("refuses to parse a corrupt store rather than losing every token silently", func() {
		Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(BeNil())
		Expect(os.WriteFile(path, []byte("not json"), 0o600)).To(BeNil())
		store := pkg.NewJSONTokenStore(path)

		_, err := store.Get(ctx, pkg.WorkerID("w1"))

		Expect(err).NotTo(BeNil())
	})
})
