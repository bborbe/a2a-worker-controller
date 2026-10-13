// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	agentv1 "github.com/bborbe/agent-task-executor/k8s/apis/agent.benjamin-borbe.de/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// The template's own values. The token is a real-looking value only so a spec can prove it is
// replaced; the shared credential stands in for the template Secret's other keys.
const (
	testTemplateName   = "claude-interactive"
	testTemplateSecret = "claude-interactive"
	testSharedKey      = "ANTHROPIC_AUTH_TOKEN"
	testSharedValue    = "shared-provider-credential"
	testTemplateToken  = "template-shared-bearer"
	testWorkerToken    = "worker-own-bearer"
	testNamespace      = "dev"
	testIngressDomain  = "dev.nuke.example"
	testTLSSecretName  = "tls-nuke-dev"
	testCreatedUID     = types.UID("uid-of-created-config")
	testImage          = "registry/bborbe/agent-claude:v0.19.1"
)

// testKubernetesConfig is the backend configuration every k8s spec runs with.
func testKubernetesConfig() pkg.KubernetesConfig {
	config := pkg.DefaultKubernetesConfig()
	config.Namespace = testNamespace
	config.IngressDomain = testIngressDomain
	config.TLSSecretName = testTLSSecretName
	config.EgressAllow = []pkg.EgressDestination{
		{Namespace: "dev", Port: 8788},
		{Namespace: "monitoring", Port: 9090},
	}
	return config
}

// templateConfig is a Config shaped like the chart's claude-interactive entry, including the
// three routing fields a worker must not inherit.
func templateConfig() agentv1.Config {
	return agentv1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: testTemplateName, Namespace: testNamespace},
		Spec: agentv1.ConfigSpec{
			Assignee:          testTemplateName,
			Type:              agentv1.AgentTypeService,
			Image:             testImage,
			Heartbeat:         "5m",
			PriorityClassName: "claude-interactive",
			SecretName:        testTemplateSecret,
			TaskType:          "llm", //nolint:staticcheck // the field a clone must clear
			TaskTypes:         []string{"llm"},
			Trigger:           &agentv1.Trigger{},
			Env: map[string]string{
				"ANTHROPIC_BASE_URL": "http://claude-code-router-dev:8788",
				"A2A_PUBLIC_URL":     "https://claude-interactive.dev.nuke.example/a2a",
				"A2A_AGENT_NAME":     "claude-interactive",
			},
		},
	}
}

// templateSecret is the template's credential Secret.
func templateSecret() corev1.Secret {
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testTemplateSecret, Namespace: testNamespace},
		Data: map[string][]byte{
			testSharedKey:    []byte(testSharedValue),
			testTokenEnvName: []byte(testTemplateToken),
		},
	}
}

// storedWorkerConfig is a worker Config as the API server returns it after create: the
// builder's output plus the uid the owner references point at.
func storedWorkerConfig(builder pkg.WorkerResourceBuilder) agentv1.Config {
	config := builder.BuildConfig(templateConfig(), "abcd", "Build the k3s Backend")
	config.UID = testCreatedUID
	return *config
}

var _ = Describe("WorkerResourceBuilder", func() {
	var builder pkg.WorkerResourceBuilder

	BeforeEach(func() {
		builder = pkg.NewWorkerResourceBuilder(testKubernetesConfig())
	})

	Describe("BuildConfig", func() {
		var (
			template agentv1.Config
			config   *agentv1.Config
		)

		BeforeEach(func() {
			template = templateConfig()
			config = builder.BuildConfig(template, "abcd", "Build the k3s Backend")
		})

		It("names the worker after its id and makes it its own assignee", func() {
			Expect(config.Name).To(Equal("a2a-worker-abcd"))
			Expect(config.Namespace).To(Equal(testNamespace))
			Expect(config.Spec.Assignee).To(Equal("a2a-worker-abcd"))
			Expect(config.Spec.SecretName).To(Equal("a2a-worker-abcd"))
		})

		Context("with a worker priority class", func() {
			BeforeEach(func() {
				kubernetesConfig := testKubernetesConfig()
				kubernetesConfig.WorkerPriorityClass = "a2a-worker"
				config = pkg.NewWorkerResourceBuilder(kubernetesConfig).
					BuildConfig(template, "abcd", "Build the k3s Backend")
			})

			It("runs the worker under its own class, not the template's quota", func() {
				Expect(config.Spec.PriorityClassName).To(Equal("a2a-worker"))
				Expect(template.Spec.PriorityClassName).To(Equal("claude-interactive"))
			})
		})

		It("keeps the template's image, type and priority class", func() {
			Expect(config.Spec.Image).To(Equal(testImage))
			Expect(config.Spec.Type).To(Equal(agentv1.AgentTypeService))
			Expect(config.Spec.PriorityClassName).To(Equal("claude-interactive"))
			Expect(config.Spec.Heartbeat).To(Equal("5m"))
		})

		It("neutralises every routing field, so the executor never sends the worker vault tasks",
			func() {
				// Never empty: the CRD rejects "" (no omitempty on the tag). The worker's own
				// name is a valid task type no vault task carries.
				Expect(
					config.Spec.TaskType,
				).To(Equal("a2a-worker-abcd"))
				//nolint:staticcheck // asserting it is cleared
				Expect(config.Spec.TaskTypes).To(BeNil())
				Expect(config.Spec.Trigger).To(BeNil())
			},
		)

		It("overrides the card identity and keeps the rest of the env", func() {
			Expect(config.Spec.Env).To(HaveKeyWithValue(
				"A2A_PUBLIC_URL", "https://a2a-worker-abcd."+testIngressDomain+"/a2a",
			))
			Expect(
				config.Spec.Env,
			).To(HaveKeyWithValue("A2A_AGENT_NAME", "claude-interactive-abcd"))
			Expect(config.Spec.Env).To(HaveKeyWithValue(
				"ANTHROPIC_BASE_URL", "http://claude-code-router-dev:8788",
			))
		})

		It("leaves the template untouched", func() {
			Expect(template.Spec.Assignee).To(Equal(testTemplateName))
			Expect(template.Spec.TaskTypes).To(Equal([]string{"llm"}))
			Expect(template.Spec.Trigger).NotTo(BeNil())
			Expect(template.Spec.Env).To(HaveKeyWithValue("A2A_AGENT_NAME", "claude-interactive"))
		})

		It("marks the Config with the controller label and carries the task as an annotation",
			func() {
				Expect(config.Labels).To(Equal(map[string]string{
					testControllerLabel: testControllerLabelValue,
					testWorkerIDLabel:   "abcd",
				}))
				Expect(config.Annotations).To(Equal(map[string]string{
					testTaskLabel: "Build the k3s Backend",
				}))
			},
		)
	})

	Describe("BuildSecret", func() {
		var (
			template corev1.Secret
			secret   *corev1.Secret
		)

		BeforeEach(func() {
			template = templateSecret()
			secret = builder.BuildSecret(storedWorkerConfig(builder), template, testWorkerToken)
		})

		It("copies the template's credentials but replaces the bearer with the worker's own",
			func() {
				Expect(secret.Data).To(Equal(map[string][]byte{
					testSharedKey:    []byte(testSharedValue),
					testTokenEnvName: []byte(testWorkerToken),
				}))
				Expect(template.Data[testTokenEnvName]).To(Equal([]byte(testTemplateToken)))
			},
		)

		It("is named after the worker", func() {
			Expect(secret.Name).To(Equal("a2a-worker-abcd"))
			Expect(secret.Namespace).To(Equal(testNamespace))
		})

		It("never carries the token in a label or an annotation", func() {
			for _, value := range secret.Labels {
				Expect(value).NotTo(ContainSubstring(testWorkerToken))
			}
			for _, value := range secret.Annotations {
				Expect(value).NotTo(ContainSubstring(testWorkerToken))
			}
		})
	})

	Describe("owned objects", func() {
		It("makes the Config the controlling owner of every object a worker is made of", func() {
			owner := storedWorkerConfig(builder)
			objects := []metav1.Object{
				builder.BuildSecret(owner, templateSecret(), testWorkerToken),
				builder.BuildService(owner),
				builder.BuildIngress(owner),
				builder.BuildNetworkPolicy(owner),
			}
			for _, object := range objects {
				owners := object.GetOwnerReferences()
				Expect(owners).To(HaveLen(1))
				Expect(owners[0].Kind).To(Equal("Config"))
				Expect(owners[0].APIVersion).To(Equal("agent.benjamin-borbe.de/v1"))
				Expect(owners[0].Name).To(Equal("a2a-worker-abcd"))
				Expect(owners[0].UID).To(Equal(testCreatedUID))
				Expect(*owners[0].Controller).To(BeTrue())
				Expect(object.GetLabels()).To(HaveKeyWithValue(
					testControllerLabel, testControllerLabelValue,
				))
				Expect(object.GetAnnotations()).To(HaveKeyWithValue(
					testTaskLabel, "Build the k3s Backend",
				))
			}
		})
	})

	Describe("BuildService", func() {
		It("selects the worker pod by its app label on the named port", func() {
			service := builder.BuildService(storedWorkerConfig(builder))
			Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(service.Spec.Selector).To(Equal(map[string]string{"app": "a2a-worker-abcd"}))
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(BeEquivalentTo(9090))
			Expect(service.Spec.Ports[0].Name).To(Equal("http"))
			Expect(service.Spec.Ports[0].TargetPort.StrVal).To(Equal("http"))
		})
	})

	Describe("BuildIngress", func() {
		It("exposes the worker over TLS on its own host", func() {
			ingress := builder.BuildIngress(storedWorkerConfig(builder))
			host := "a2a-worker-abcd." + testIngressDomain
			Expect(ingress.Spec.Rules).To(HaveLen(1))
			Expect(ingress.Spec.Rules[0].Host).To(Equal(host))
			Expect(ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name).To(
				Equal("a2a-worker-abcd"),
			)
			Expect(ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name).To(Equal("http"))
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
	})

	Describe("BuildNetworkPolicy", func() {
		var policy *networkingv1.NetworkPolicy

		BeforeEach(func() {
			policy = builder.BuildNetworkPolicy(storedWorkerConfig(builder))
		})

		It("selects only this worker's pod, by the assignee label the executor stamps", func() {
			Expect(policy.Spec.PodSelector.MatchLabels).To(Equal(map[string]string{
				"agent.benjamin-borbe.de/assignee": "a2a-worker-abcd",
			}))
		})

		It("restricts egress only, so the Ingress stays reachable", func() {
			Expect(policy.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			}))
			Expect(policy.Spec.Ingress).To(BeEmpty())
		})

		It("allows DNS on UDP and TCP 53 in kube-system", func() {
			dns := policy.Spec.Egress[0]
			Expect(dns.To[0].NamespaceSelector.MatchLabels).To(Equal(map[string]string{
				"kubernetes.io/metadata.name": "kube-system",
			}))
			Expect(dns.Ports).To(HaveLen(2))
			Expect(*dns.Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))
			Expect(dns.Ports[0].Port.IntVal).To(BeEquivalentTo(53))
			Expect(*dns.Ports[1].Protocol).To(Equal(corev1.ProtocolTCP))
		})

		It("allows each configured destination and nothing else", func() {
			Expect(policy.Spec.Egress).To(HaveLen(3))
			for i, want := range []struct {
				namespace string
				port      int32
			}{{"dev", 8788}, {"monitoring", 9090}} {
				rule := policy.Spec.Egress[i+1]
				Expect(rule.To).To(HaveLen(1))
				Expect(rule.To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
					"kubernetes.io/metadata.name", want.namespace,
				))
				Expect(rule.Ports).To(HaveLen(1))
				Expect(*rule.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
				Expect(rule.Ports[0].Port.IntVal).To(Equal(want.port))
			}
		})
	})

	Describe("A2AURL", func() {
		It("is the URL BuildConfig advertises on the card", func() {
			config := builder.BuildConfig(templateConfig(), "abcd", "task")
			Expect(builder.A2AURL("a2a-worker-abcd")).To(Equal(config.Spec.Env["A2A_PUBLIC_URL"]))
		})
	})
})
