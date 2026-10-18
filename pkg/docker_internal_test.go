// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("createRun", func() {
	var config DockerConfig

	BeforeEach(func() {
		config = DefaultDockerConfig()
		config.Image = "example/worker:v1"
	})

	run := func(mounts []Mount, secrets []SecretMount) []string {
		args, _ := createRun(
			config, WorkerID("aaaaaaaabbbbbbbb"), TaskName("a task"), 9093, "token", nil,
			mounts, secrets,
		)
		return args
	}

	It("emits no -v argument when nothing is declared", func() {
		Expect(run(nil, nil)).NotTo(ContainElement("-v"))
	})

	It("mounts a declared host path", func() {
		args := run([]Mount{{HostPath: "/srv/data", ContainerPath: "/vault"}}, nil)
		Expect(args).To(ContainElement("-v"))
		Expect(args).To(ContainElement("/srv/data:/vault"))
	})

	It("appends :ro to a read-only declared mount", func() {
		args := run([]Mount{
			{HostPath: "/srv/data", ContainerPath: "/vault", ReadOnly: true},
		}, nil)
		Expect(args).To(ContainElement("/srv/data:/vault:ro"))
	})

	It("leaves a mount writable when read_only is unset", func() {
		args := run([]Mount{{HostPath: "/srv/data", ContainerPath: "/vault"}}, nil)
		Expect(args).NotTo(ContainElement("/srv/data:/vault:ro"))
	})

	// A secret mount is ALWAYS read-only: a writable one would let a compromised worker rewrite
	// the file the next worker for that task mounts.
	It("mounts a secret read-only under the container secret directory", func() {
		args := run(nil, []SecretMount{
			{HostPath: "/host/secrets/aaaa/GATEWAY_SECRET", Name: "GATEWAY_SECRET"},
		})
		Expect(args).To(ContainElement(
			"/host/secrets/aaaa/GATEWAY_SECRET:" + SecretContainerDir + "/GATEWAY_SECRET:ro",
		))
	})

	It("mounts every declared path", func() {
		args := run(
			[]Mount{
				{HostPath: "/srv/data", ContainerPath: "/vault"},
				{HostPath: "/srv/other", ContainerPath: "/other", ReadOnly: true},
			},
			[]SecretMount{{HostPath: "/host/secrets/aaaa/TOKEN", Name: "TOKEN"}},
		)
		Expect(args).To(ContainElement("/srv/data:/vault"))
		Expect(args).To(ContainElement("/srv/other:/other:ro"))
		Expect(args).To(ContainElement(
			"/host/secrets/aaaa/TOKEN:" + SecretContainerDir + "/TOKEN:ro",
		))
	})

	// The platform is pinned in argv rather than left to the daemon's host-platform resolution.
	// On an arm64 host an amd64-only image otherwise fails BEFORE pulling, with `no matching
	// manifest for linux/arm64/v8` — and only once the image is absent from the local store,
	// which is why a cached image hides the defect.
	It("runs the worker on the build platform", func() {
		args := run(nil, nil)
		index := -1
		for i, arg := range args {
			if arg == "--platform" {
				index = i
			}
		}
		Expect(index).To(BeNumerically(">=", 0))
		Expect(args[index+1]).To(Equal("linux/amd64"))
	})

	// The image closes the argv: anything appended after it would be read as the container's
	// command rather than as a run flag.
	It("keeps the image last", func() {
		args := run(
			[]Mount{{HostPath: "/srv/data", ContainerPath: "/vault"}},
			[]SecretMount{{HostPath: "/host/secrets/aaaa/TOKEN", Name: "TOKEN"}},
		)
		Expect(args[len(args)-1]).To(Equal(config.Image))
	})

	// The value travels in the child environment, never in argv — the property that keeps a
	// credential out of `ps`.
	It("never puts a secret value in argv", func() {
		args := run(nil, []SecretMount{{HostPath: "/host/secrets/aaaa/TOKEN", Name: "TOKEN"}})
		Expect(args).NotTo(ContainElement(ContainSubstring("canary")))
	})
})
