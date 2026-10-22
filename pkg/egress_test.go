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

	It("parses a pod-label-scoped destination", func() {
		destinations, err := pkg.ParseEgressDestinations(
			ctx,
			"dev:8788,dev/app=vault-obsidian-personal:9090",
		)
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{Namespace: "dev", Port: 8788},
			{
				Namespace: "dev",
				PodLabels: map[string]string{"app": "vault-obsidian-personal"},
				Port:      9090,
			},
		}))
	})

	It("reads an empty value as no destinations", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "")
		Expect(err).To(BeNil())
		Expect(destinations).To(BeEmpty())
	})

	It("parses an external destination as a CIDR alongside an in-cluster one", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "dev:8788,cidr=140.82.112.0/20:443")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{Namespace: "dev", Port: 8788},
			{IPBlock: "140.82.112.0/20", Port: 443},
		}))
	})

	It("normalises a CIDR to its network address", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "cidr=140.82.112.5/20:443")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{IPBlock: "140.82.112.0/20", Port: 443},
		}))
	})

	It("parses both spellings of one CIDR to the same destination, without deduplicating", func() {
		// Both entries survive: normalisation makes them the SAME rule, it does not merge them.
		// Asserted rather than assumed, because a reader could take "normalised" to mean
		// "deduplicated" and rely on a count the parser never promises.
		destinations, err := pkg.ParseEgressDestinations(
			ctx,
			"cidr=140.82.112.5/20:443,cidr=140.82.112.0/20:443",
		)
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{IPBlock: "140.82.112.0/20", Port: 443},
			{IPBlock: "140.82.112.0/20", Port: 443},
		}))
	})

	It("parses an IPv6 CIDR, splitting the port at the last colon", func() {
		// The address itself contains colons, so a first-colon split would cut it in half and
		// report "no valid CIDR" for a CIDR that is valid.
		destinations, err := pkg.ParseEgressDestinations(ctx, "cidr=2001:db8::/32:443")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{IPBlock: "2001:db8::/32", Port: 443},
		}))
	})

	It("keeps a /32 CIDR as the single address it names", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "cidr=10.43.0.1/32:443")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{IPBlock: "10.43.0.1/32", Port: 443},
		}))
	})

	It("keeps a /0 CIDR verbatim, leaving the blast radius to the operator", func() {
		destinations, err := pkg.ParseEgressDestinations(ctx, "cidr=0.0.0.0/0:443")
		Expect(err).To(BeNil())
		Expect(destinations).To(Equal([]pkg.EgressDestination{
			{IPBlock: "0.0.0.0/0", Port: 443},
		}))
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
		Entry("label without value", "dev/app=:9090"),
		Entry("label without key", "dev/=vault:9090"),
		Entry("label without equals", "dev/app:9090"),
		Entry("cidr without port", "cidr=140.82.112.0/20"),
		Entry("cidr empty", "cidr=:443"),
		Entry("cidr not a cidr", "cidr=github.com:443"),
		Entry("cidr with no mask", "cidr=140.82.112.0:443"),
		Entry("cidr with port zero", "cidr=140.82.112.0/20:0"),
		Entry("ipv6 cidr without port", "cidr=2001:db8::/32"),
	)
})
