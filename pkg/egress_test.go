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

var _ = Describe("ParseEgressDestinations", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("parses a namespace:port list", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "dev:8788, monitoring:9090")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{Namespace: "dev", Port: 8788},
			{Namespace: "monitoring", Port: 9090},
		}))
	})

	It("reads an empty value as no destinations", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "")
		Expect(err).To(BeNil())
		Expect(destinations).To(BeEmpty())
	})

	DescribeTable("refuses a malformed entry rather than dropping it",
		func(value string) {
			_, err := pkg.ParseEgressDestinations(ctx, value)
			Expect(err).NotTo(BeNil())
		},
		Entry("no port", "dev"),
		Entry("no namespace", ":8788"),
		Entry("port not a number", "dev:http"),
		Entry("port zero", "dev:0"),
		Entry("port out of range", "dev:70000"),
	)
})
