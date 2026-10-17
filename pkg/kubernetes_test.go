// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"errors"

	agentv1 "github.com/bborbe/agent-task-executor/k8s/apis/agent.benjamin-borbe.de/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg"
)

// errCluster stands in for an API server failure.
var errCluster = errors.New("api server unavailable")

// ownedWorkerConfig is a worker Config as the backend writes it, so list and stop specs read
// the same shape create produces.
func ownedWorkerConfig(workerID pkg.WorkerID, task pkg.TaskName) agentv1.Config {
	return agentv1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a2a-worker-" + string(workerID),
			Labels: map[string]string{
				testControllerLabel: testControllerLabelValue,
				testWorkerIDLabel:   string(workerID),
			},
			Annotations: map[string]string{testTaskLabel: string(task)},
		},
	}
}

var _ = Describe("KubernetesBackend", func() {
	var (
		ctx     context.Context
		client  *mocks.PkgKubernetesClient
		backend pkg.WorkerBackend
	)

	BeforeEach(func() {
		ctx = context.Background()
		client = &mocks.PkgKubernetesClient{}
		template := templateConfig()
		client.GetConfigReturns(&template, nil)
		secret := templateSecret()
		client.GetSecretReturns(&secret, nil)
		// The API server returns the Config it stored, stamped with the uid every owned
		// object's owner reference must point at.
		client.CreateConfigStub = func(
			_ context.Context,
			config *agentv1.Config,
		) (*agentv1.Config, error) {
			stored := config.DeepCopy()
			stored.UID = testCreatedUID
			return stored, nil
		}
		backend = pkg.NewKubernetesBackend(
			client,
			pkg.NewWorkerResourceBuilder(testKubernetesConfig()),
			testTemplateName,
			nil,
		)
	})

	Describe("Create", func() {
		var (
			response *pkg.CreateResponse
			err      error
		)

		JustBeforeEach(func() {
			response, err = backend.Create(ctx, pkg.CreateRequest{Task: "Build the k3s Backend"})
		})

		It("reads the template Config and its Secret", func() {
			Expect(err).To(BeNil())
			Expect(client.GetConfigCallCount()).To(Equal(1))
			_, name := client.GetConfigArgsForCall(0)
			Expect(name).To(Equal(testTemplateName))
			Expect(client.GetSecretCallCount()).To(Equal(1))
			_, secretName := client.GetSecretArgsForCall(0)
			Expect(secretName).To(Equal(testTemplateSecret))
		})

		It("writes one Config and its three owned objects", func() {
			Expect(err).To(BeNil())
			Expect(client.CreateConfigCallCount()).To(Equal(1))
			Expect(client.CreateSecretCallCount()).To(Equal(1))
			Expect(client.CreateServiceCallCount()).To(Equal(1))
			Expect(client.CreateIngressCallCount()).To(Equal(1))
			Expect(client.CreateNetworkPolicyCallCount()).To(Equal(1))
			Expect(client.DeleteConfigCallCount()).To(Equal(0))
		})

		It("returns a worker on the k8s backend with its own token", func() {
			Expect(err).To(BeNil())
			Expect(response.Worker.Backend).To(Equal(pkg.BackendKubernetes))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateRunning))
			Expect(response.Worker.Task).To(Equal(pkg.TaskName("Build the k3s Backend")))
			Expect(response.Token).To(HaveLen(48))
			Expect(response.Token).NotTo(Equal(testTemplateToken))
		})

		It("writes the token into the worker Secret and nowhere else", func() {
			Expect(err).To(BeNil())
			_, secret := client.CreateSecretArgsForCall(0)
			Expect(secret.Data[testTokenEnvName]).To(Equal([]byte(response.Token)))
			_, config := client.CreateConfigArgsForCall(0)
			for _, value := range config.Spec.Env {
				Expect(value).NotTo(ContainSubstring(response.Token))
			}
		})

		It("names the Config after the returned worker id", func() {
			Expect(err).To(BeNil())
			_, config := client.CreateConfigArgsForCall(0)
			Expect(config.Name).To(Equal("a2a-worker-" + string(response.Worker.WorkerID)))
			Expect(response.Worker.A2AURL).To(Equal(config.Spec.Env["A2A_PUBLIC_URL"]))
		})

		It("owns every child by the stored Config's uid", func() {
			Expect(err).To(BeNil())
			_, secret := client.CreateSecretArgsForCall(0)
			_, service := client.CreateServiceArgsForCall(0)
			_, ingress := client.CreateIngressArgsForCall(0)
			_, policy := client.CreateNetworkPolicyArgsForCall(0)
			for _, object := range []metav1.Object{secret, service, ingress, policy} {
				Expect(object.GetOwnerReferences()).To(HaveLen(1))
				Expect(object.GetOwnerReferences()[0].UID).To(Equal(testCreatedUID))
			}
		})

		Context("when the task already has a worker", func() {
			BeforeEach(func() {
				client.ListConfigsReturns([]agentv1.Config{
					ownedWorkerConfig("existing", "Build the k3s Backend"),
				}, nil)
			})

			It("refuses, naming the existing worker's id, and creates nothing", func() {
				// ⚠️ The id is what makes the refusal actionable: `stop` resolves ownership from
				// it alone, so a caller that does not receive it cannot reap the worker it is
				// being told about.
				Expect(err).To(MatchError(ContainSubstring("existing")))
				Expect(response).To(BeNil())
				Expect(client.CreateConfigCallCount()).To(Equal(0))
				Expect(client.CreateSecretCallCount()).To(Equal(0))
			})
		})

		Context("when the template names no Secret", func() {
			BeforeEach(func() {
				template := templateConfig()
				template.Spec.SecretName = ""
				client.GetConfigReturns(&template, nil)
			})

			It("refuses before writing anything", func() {
				Expect(err).NotTo(BeNil())
				Expect(client.CreateConfigCallCount()).To(Equal(0))
			})
		})

		Context("when writing the network policy fails", func() {
			BeforeEach(func() {
				client.CreateNetworkPolicyReturns(errCluster)
			})

			It("never writes the Secret, so no worker pod can start unconfined", func() {
				Expect(err).NotTo(BeNil())
				Expect(client.CreateSecretCallCount()).To(Equal(0))
				Expect(client.DeleteConfigCallCount()).To(Equal(1))
			})
		})

		Context("when writing an owned object fails", func() {
			BeforeEach(func() {
				client.CreateServiceReturns(errCluster)
			})

			It("deletes the Config so nothing half-built is left behind", func() {
				Expect(err).NotTo(BeNil())
				Expect(response).To(BeNil())
				Expect(client.DeleteConfigCallCount()).To(Equal(1))
				_, created := client.CreateConfigArgsForCall(0)
				_, deleted := client.DeleteConfigArgsForCall(0)
				Expect(deleted).To(Equal(created.Name))
				Expect(client.CreateIngressCallCount()).To(Equal(0))
			})
		})

		Context("when listing fails", func() {
			BeforeEach(func() {
				client.ListConfigsReturns(nil, errCluster)
			})

			It("returns the error and writes nothing", func() {
				Expect(err).NotTo(BeNil())
				Expect(client.CreateConfigCallCount()).To(Equal(0))
			})
		})
	})

	Describe("List", func() {
		It("filters on the controller label", func() {
			_, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(client.ListConfigsCallCount()).To(Equal(1))
			_, selector := client.ListConfigsArgsForCall(0)
			Expect(selector).To(Equal(testControllerLabel + "=" + testControllerLabelValue))
		})

		It("rebuilds every owned worker from its Config", func() {
			client.ListConfigsReturns([]agentv1.Config{
				ownedWorkerConfig("aaaa", "task a"),
				ownedWorkerConfig("bbbb", "task b"),
			}, nil)
			response, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(2))
			Expect(response.Workers[0].WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			Expect(response.Workers[0].Task).To(Equal(pkg.TaskName("task a")))
			Expect(response.Workers[0].Backend).To(Equal(pkg.BackendKubernetes))
			Expect(response.Workers[0].A2AURL).To(Equal(
				"https://a2a-worker-aaaa." + testIngressDomain + "/a2a",
			))
			Expect(response.Workers[1].WorkerID).To(Equal(pkg.WorkerID("bbbb")))
		})

		It("never returns a Config the chart rendered, even if the client ignored the selector",
			func() {
				chart := templateConfig()
				client.ListConfigsReturns(
					[]agentv1.Config{chart, ownedWorkerConfig("aaaa", "task a")},
					nil,
				)
				response, err := backend.List(ctx)
				Expect(err).To(BeNil())
				Expect(response.Workers).To(HaveLen(1))
				Expect(response.Workers[0].WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			},
		)

		It("skips a partially labelled Config", func() {
			partial := ownedWorkerConfig("aaaa", "task a")
			partial.Annotations = nil
			client.ListConfigsReturns([]agentv1.Config{partial}, nil)
			response, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
		})

		It("returns the client's error", func() {
			client.ListConfigsReturns(nil, errCluster)
			_, err := backend.List(ctx)
			Expect(err).NotTo(BeNil())
		})
	})

	Describe("Stop", func() {
		BeforeEach(func() {
			client.ListConfigsReturns([]agentv1.Config{
				templateConfig(),
				ownedWorkerConfig("aaaa", "task a"),
			}, nil)
		})

		It("deletes the owned worker's Config and reports it stopped", func() {
			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "aaaa"})
			Expect(err).To(BeNil())
			Expect(client.DeleteConfigCallCount()).To(Equal(1))
			_, name := client.DeleteConfigArgsForCall(0)
			Expect(name).To(Equal("a2a-worker-aaaa"))
			Expect(response.Worker.WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
		})

		It("refuses the chart's own Config by name and deletes nothing", func() {
			_, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: testTemplateName})
			Expect(err).To(MatchError(ContainSubstring("not owned")))
			Expect(client.DeleteConfigCallCount()).To(Equal(0))
		})

		It("returns the client's delete error", func() {
			client.DeleteConfigReturns(errCluster)
			_, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "aaaa"})
			Expect(err).NotTo(BeNil())
		})
	})
})
