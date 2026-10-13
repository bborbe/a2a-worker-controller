// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// errCluster stands in for an API server failure.
var errCluster = errors.New("api server unavailable")

// The template's own values. The token is a real-looking value only so a spec can prove it is
// replaced; the shared credential stands in for the template Secret's other keys.
const (
	testTemplateName   = "claude-interactive"
	testTemplateSecret = "claude-interactive"
	testSharedKey      = "ANTHROPIC_AUTH_TOKEN"
	testSharedValue    = "shared-provider-credential"
	testTemplateToken  = "template-shared-bearer"
	testNamespace      = "dev"
	testIngressDomain  = "dev.nuke.example"
	testTLSSecretName  = "tls-nuke-dev"
	testCreatedUID     = types.UID("uid-of-created-config")
)

// fakeKubernetesClient is the injected KubernetesClient seam. It stores what the backend
// writes and answers reads from that store, so a spec drives the real backend end to end
// without a cluster.
type fakeKubernetesClient struct {
	template       *unstructured.Unstructured
	templateSecret *corev1.Secret
	configs        []unstructured.Unstructured

	createdConfigs  []*unstructured.Unstructured
	createdSecrets  []*corev1.Secret
	createdServices []*corev1.Service
	createdIngress  []*networkingv1.Ingress
	deletedConfigs  []string
	listSelectors   []string

	listErr          error
	createServiceErr error
}

func (f *fakeKubernetesClient) GetConfig(
	_ context.Context,
	name string,
) (*unstructured.Unstructured, error) {
	if name == f.template.GetName() {
		return f.template.DeepCopy(), nil
	}
	return nil, errCluster
}

func (f *fakeKubernetesClient) ListConfigs(
	_ context.Context,
	labelSelector string,
) ([]unstructured.Unstructured, error) {
	f.listSelectors = append(f.listSelectors, labelSelector)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.configs, nil
}

func (f *fakeKubernetesClient) CreateConfig(
	_ context.Context,
	config *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	f.createdConfigs = append(f.createdConfigs, config)
	stored := config.DeepCopy()
	stored.SetUID(testCreatedUID)
	return stored, nil
}

func (f *fakeKubernetesClient) DeleteConfig(_ context.Context, name string) error {
	f.deletedConfigs = append(f.deletedConfigs, name)
	return nil
}

func (f *fakeKubernetesClient) GetSecret(_ context.Context, name string) (*corev1.Secret, error) {
	if name == f.templateSecret.Name {
		return f.templateSecret.DeepCopy(), nil
	}
	return nil, errCluster
}

func (f *fakeKubernetesClient) CreateSecret(_ context.Context, secret *corev1.Secret) error {
	f.createdSecrets = append(f.createdSecrets, secret)
	return nil
}

func (f *fakeKubernetesClient) CreateService(_ context.Context, service *corev1.Service) error {
	f.createdServices = append(f.createdServices, service)
	return f.createServiceErr
}

func (f *fakeKubernetesClient) CreateIngress(
	_ context.Context,
	ingress *networkingv1.Ingress,
) error {
	f.createdIngress = append(f.createdIngress, ingress)
	return nil
}

// templateConfig is a Config shaped like the chart's claude-interactive entry, including the
// two routing fields a worker must not inherit.
func templateConfig() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agent.benjamin-borbe.de/v1",
		"kind":       "Config",
		"metadata": map[string]interface{}{
			"name":      testTemplateName,
			"namespace": testNamespace,
		},
		"spec": map[string]interface{}{
			"assignee":          testTemplateName,
			"type":              "service",
			"image":             "registry/bborbe/agent-claude:v0.19.1",
			"heartbeat":         "5m",
			"priorityClassName": "claude-interactive",
			"secretName":        testTemplateSecret,
			"taskTypes":         []interface{}{"llm"},
			"trigger":           map[string]interface{}{"phases": []interface{}{"planning"}},
			"env": map[string]interface{}{
				"ANTHROPIC_BASE_URL": "http://claude-code-router-dev:8788",
				"A2A_PUBLIC_URL":     "https://claude-interactive.dev.nuke.example/a2a",
				"A2A_AGENT_NAME":     "claude-interactive",
			},
		},
	}}
}

// workerConfig is a Config as the backend writes it, so list and stop specs read the same
// shape create produces.
func workerConfig(workerID pkg.WorkerID, task pkg.TaskName) unstructured.Unstructured {
	config := unstructured.Unstructured{Object: map[string]interface{}{}}
	config.SetName("a2a-worker-" + string(workerID))
	config.SetLabels(map[string]string{
		testControllerLabel: testControllerLabelValue,
		testWorkerIDLabel:   string(workerID),
	})
	config.SetAnnotations(map[string]string{testTaskLabel: string(task)})
	return config
}

var _ = Describe("KubernetesBackend", func() {
	var (
		ctx     context.Context
		client  *fakeKubernetesClient
		backend pkg.WorkerBackend
	)

	BeforeEach(func() {
		ctx = context.Background()
		client = &fakeKubernetesClient{
			template: templateConfig(),
			templateSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testTemplateSecret, Namespace: testNamespace},
				Data: map[string][]byte{
					testSharedKey:    []byte(testSharedValue),
					testTokenEnvName: []byte(testTemplateToken),
				},
			},
		}
		config := pkg.DefaultKubernetesConfig()
		config.Namespace = testNamespace
		config.IngressDomain = testIngressDomain
		config.TLSSecretName = testTLSSecretName
		backend = pkg.NewKubernetesBackend(client, config)
	})

	Describe("Create", func() {
		var (
			response *pkg.CreateResponse
			err      error
		)

		JustBeforeEach(func() {
			response, err = backend.Create(ctx, pkg.CreateRequest{Task: "Build the k3s Backend"})
		})

		It("writes one Config and its three owned objects", func() {
			Expect(err).To(BeNil())
			Expect(client.createdConfigs).To(HaveLen(1))
			Expect(client.createdSecrets).To(HaveLen(1))
			Expect(client.createdServices).To(HaveLen(1))
			Expect(client.createdIngress).To(HaveLen(1))
		})

		It("returns a worker on the k8s backend with its own token", func() {
			Expect(err).To(BeNil())
			Expect(response.Worker.Backend).To(Equal(pkg.BackendKubernetes))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateRunning))
			Expect(response.Worker.Task).To(Equal(pkg.TaskName("Build the k3s Backend")))
			Expect(response.Token).To(HaveLen(48))
			Expect(response.Token).NotTo(Equal(testTemplateToken))
		})

		It("names the worker after its id and makes it its own assignee", func() {
			Expect(err).To(BeNil())
			name := "a2a-worker-" + string(response.Worker.WorkerID)
			config := client.createdConfigs[0]
			Expect(config.GetName()).To(Equal(name))
			Expect(config.GetNamespace()).To(Equal(testNamespace))
			assignee, _, _ := unstructured.NestedString(config.Object, "spec", "assignee")
			Expect(assignee).To(Equal(name))
			secretName, _, _ := unstructured.NestedString(config.Object, "spec", "secretName")
			Expect(secretName).To(Equal(name))
		})

		It("keeps the template's image and priority class", func() {
			Expect(err).To(BeNil())
			spec, found, specErr := unstructured.NestedMap(client.createdConfigs[0].Object, "spec")
			Expect(specErr).To(BeNil())
			Expect(found).To(BeTrue())
			Expect(spec["image"]).To(Equal("registry/bborbe/agent-claude:v0.19.1"))
			Expect(spec["priorityClassName"]).To(Equal("claude-interactive"))
			Expect(spec["type"]).To(Equal("service"))
		})

		It("drops the routing fields, so the executor never sends the worker vault tasks", func() {
			Expect(err).To(BeNil())
			spec, found, specErr := unstructured.NestedMap(client.createdConfigs[0].Object, "spec")
			Expect(specErr).To(BeNil())
			Expect(found).To(BeTrue())
			Expect(spec).NotTo(HaveKey("trigger"))
			Expect(spec).NotTo(HaveKey("taskTypes"))
		})

		It("advertises the same URL in the card env and in the reply", func() {
			Expect(err).To(BeNil())
			env, _, _ := unstructured.NestedStringMap(
				client.createdConfigs[0].Object, "spec", "env",
			)
			Expect(env["A2A_PUBLIC_URL"]).To(Equal(response.Worker.A2AURL))
			Expect(response.Worker.A2AURL).To(Equal(
				"https://a2a-worker-" + string(response.Worker.WorkerID) + "." +
					testIngressDomain + "/a2a",
			))
			Expect(env["A2A_AGENT_NAME"]).To(Equal(
				"claude-interactive-" + string(response.Worker.WorkerID),
			))
			Expect(env["ANTHROPIC_BASE_URL"]).To(Equal("http://claude-code-router-dev:8788"))
		})

		It("leaves the template untouched", func() {
			Expect(err).To(BeNil())
			Expect(client.template.Object["spec"]).To(HaveKey("trigger"))
			assignee, _, _ := unstructured.NestedString(client.template.Object, "spec", "assignee")
			Expect(assignee).To(Equal(testTemplateName))
		})

		It("marks the Config with the controller label and carries the task as an annotation",
			func() {
				Expect(err).To(BeNil())
				config := client.createdConfigs[0]
				Expect(config.GetLabels()).To(HaveKeyWithValue(
					testControllerLabel, testControllerLabelValue,
				))
				Expect(config.GetLabels()).To(HaveKeyWithValue(
					testWorkerIDLabel, string(response.Worker.WorkerID),
				))
				Expect(config.GetLabels()).NotTo(HaveKey(testTaskLabel))
				Expect(config.GetAnnotations()).To(HaveKeyWithValue(
					testTaskLabel, "Build the k3s Backend",
				))
			},
		)

		It("copies the template's credentials but replaces the bearer with the worker's own",
			func() {
				Expect(err).To(BeNil())
				secret := client.createdSecrets[0]
				Expect(secret.Data).To(HaveKeyWithValue(testSharedKey, []byte(testSharedValue)))
				Expect(secret.Data).To(HaveKeyWithValue(
					testTokenEnvName, []byte(response.Token),
				))
				Expect(client.templateSecret.Data[testTokenEnvName]).To(
					Equal([]byte(testTemplateToken)),
				)
			},
		)

		It("never writes the token into a label or an annotation", func() {
			Expect(err).To(BeNil())
			objects := []metav1.Object{
				client.createdConfigs[0],
				client.createdSecrets[0],
				client.createdServices[0],
				client.createdIngress[0],
			}
			for _, object := range objects {
				for _, value := range object.GetLabels() {
					Expect(value).NotTo(ContainSubstring(response.Token))
				}
				for _, value := range object.GetAnnotations() {
					Expect(value).NotTo(ContainSubstring(response.Token))
				}
			}
		})

		It("makes the Config the controlling owner of every object it creates", func() {
			Expect(err).To(BeNil())
			objects := []metav1.Object{
				client.createdSecrets[0],
				client.createdServices[0],
				client.createdIngress[0],
			}
			for _, object := range objects {
				owners := object.GetOwnerReferences()
				Expect(owners).To(HaveLen(1))
				Expect(owners[0].Kind).To(Equal("Config"))
				Expect(owners[0].APIVersion).To(Equal("agent.benjamin-borbe.de/v1"))
				Expect(owners[0].UID).To(Equal(testCreatedUID))
				Expect(*owners[0].Controller).To(BeTrue())
			}
		})

		It("selects the worker pod by its app label on the named port", func() {
			Expect(err).To(BeNil())
			service := client.createdServices[0]
			name := "a2a-worker-" + string(response.Worker.WorkerID)
			Expect(service.Spec.Selector).To(Equal(map[string]string{"app": name}))
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(BeEquivalentTo(9090))
			Expect(service.Spec.Ports[0].Name).To(Equal("http"))
		})

		It("exposes the worker over TLS on its own host", func() {
			Expect(err).To(BeNil())
			ingress := client.createdIngress[0]
			host := "a2a-worker-" + string(response.Worker.WorkerID) + "." + testIngressDomain
			Expect(ingress.Spec.Rules).To(HaveLen(1))
			Expect(ingress.Spec.Rules[0].Host).To(Equal(host))
			Expect(ingress.Spec.TLS).To(HaveLen(1))
			Expect(ingress.Spec.TLS[0].Hosts).To(Equal([]string{host}))
			Expect(ingress.Spec.TLS[0].SecretName).To(Equal(testTLSSecretName))
			Expect(*ingress.Spec.IngressClassName).To(Equal("traefik"))
			Expect(ingress.Annotations).To(HaveKeyWithValue(
				"traefik.ingress.kubernetes.io/router.entrypoints", "websecure",
			))
			Expect(ingress.Annotations).To(HaveKeyWithValue(
				"traefik.ingress.kubernetes.io/router.tls", "true",
			))
		})

		Context("when the task already has a worker", func() {
			BeforeEach(func() {
				client.configs = []unstructured.Unstructured{
					workerConfig("existing", "Build the k3s Backend"),
				}
			})

			It("returns the existing worker with no token and creates nothing", func() {
				Expect(err).To(BeNil())
				Expect(response.Worker.WorkerID).To(Equal(pkg.WorkerID("existing")))
				Expect(response.Token).To(BeEmpty())
				Expect(client.createdConfigs).To(BeEmpty())
				Expect(client.createdSecrets).To(BeEmpty())
			})
		})

		Context("when writing an owned object fails", func() {
			BeforeEach(func() {
				client.createServiceErr = errCluster
			})

			It("deletes the Config so nothing half-built is left behind", func() {
				Expect(err).NotTo(BeNil())
				Expect(response).To(BeNil())
				Expect(
					client.deletedConfigs,
				).To(Equal([]string{client.createdConfigs[0].GetName()}))
			})
		})
	})

	Describe("List", func() {
		It("filters on the controller label", func() {
			_, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(client.listSelectors).To(Equal([]string{
				testControllerLabel + "=" + testControllerLabelValue,
			}))
		})

		It("rebuilds every owned worker from its Config", func() {
			client.configs = []unstructured.Unstructured{
				workerConfig("aaaa", "task a"),
				workerConfig("bbbb", "task b"),
			}
			response, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(2))
			Expect(response.Workers[0].WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			Expect(response.Workers[0].Task).To(Equal(pkg.TaskName("task a")))
			Expect(response.Workers[0].Backend).To(Equal(pkg.BackendKubernetes))
			Expect(response.Workers[1].WorkerID).To(Equal(pkg.WorkerID("bbbb")))
		})

		It("never returns a Config the chart rendered, even if the client ignored the selector",
			func() {
				chart := unstructured.Unstructured{Object: map[string]interface{}{}}
				chart.SetName(testTemplateName)
				client.configs = []unstructured.Unstructured{chart, workerConfig("aaaa", "task a")}
				response, err := backend.List(ctx)
				Expect(err).To(BeNil())
				Expect(response.Workers).To(HaveLen(1))
				Expect(response.Workers[0].WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			},
		)

		It("skips a partially labelled Config", func() {
			partial := workerConfig("aaaa", "task a")
			partial.SetAnnotations(nil)
			client.configs = []unstructured.Unstructured{partial}
			response, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
		})

		It("returns the client's error", func() {
			client.listErr = errCluster
			_, err := backend.List(ctx)
			Expect(err).NotTo(BeNil())
		})
	})

	Describe("Stop", func() {
		BeforeEach(func() {
			client.configs = []unstructured.Unstructured{workerConfig("aaaa", "task a")}
		})

		It("deletes the owned worker's Config and reports it stopped", func() {
			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "aaaa"})
			Expect(err).To(BeNil())
			Expect(client.deletedConfigs).To(Equal([]string{"a2a-worker-aaaa"}))
			Expect(response.Worker.WorkerID).To(Equal(pkg.WorkerID("aaaa")))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
		})

		It("refuses a name it does not own and deletes nothing", func() {
			_, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: testTemplateName})
			Expect(err).NotTo(BeNil())
			Expect(strings.Contains(err.Error(), "not owned")).To(BeTrue())
			Expect(client.deletedConfigs).To(BeEmpty())
		})
	})
})
