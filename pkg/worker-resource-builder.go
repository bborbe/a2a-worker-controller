// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	agentv1 "github.com/bborbe/agent-task-executor/k8s/apis/agent.benjamin-borbe.de/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The k8s backend reuses the docker backend's label keys, so one ownership rule covers both
// backends. The task is an annotation rather than a label: a vault task name carries spaces
// and punctuation, which Kubernetes refuses in a label value but accepts in an annotation.
const taskAnnotation = taskLabel

// The worker's own listen port and the port name the executor's StatefulSet declares. The
// Service and Ingress address the port by this name, so they cannot drift apart on a number.
const (
	kubernetesWorkerPort     = 9090
	kubernetesWorkerPortName = "http"
)

// The labels and DNS location the egress NetworkPolicy keys on. assigneePodLabel is the pod
// label the executor copies a Config's assignee into; namespaceNameLabel is the label
// Kubernetes stamps on every namespace with its own name.
const (
	assigneePodLabel   = "agent.benjamin-borbe.de/assignee"
	namespaceNameLabel = "kubernetes.io/metadata.name"
	dnsNamespace       = "kube-system"
	dnsPort            = 53
)

//counterfeiter:generate -o ../mocks/pkg-worker-resource-builder.go --fake-name PkgWorkerResourceBuilder . WorkerResourceBuilder

// WorkerResourceBuilder renders every Kubernetes object a k8s worker is made of. It decides
// what a worker looks like; it never talks to the cluster, so every method is a pure function
// of its arguments and the backend's configuration.
type WorkerResourceBuilder interface {
	// BuildConfig clones template into the Config for one worker serving task.
	BuildConfig(template agentv1.Config, workerID WorkerID, task TaskName) *agentv1.Config
	// BuildSecret renders the worker's credential Secret, owned by owner: every key of
	// template, with the bearer replaced by token.
	BuildSecret(owner agentv1.Config, template corev1.Secret, token string) *corev1.Secret
	// BuildService renders the ClusterIP Service in front of owner's pod.
	BuildService(owner agentv1.Config) *corev1.Service
	// BuildIngress renders the TLS Ingress that exposes owner's Service on its own host.
	BuildIngress(owner agentv1.Config) *networkingv1.Ingress
	// BuildNetworkPolicy renders the egress NetworkPolicy that confines owner's pod.
	BuildNetworkPolicy(owner agentv1.Config) *networkingv1.NetworkPolicy
	// A2AURL is the address the worker named name advertises.
	A2AURL(name string) string
}

// workerResourceBuilder is the WorkerResourceBuilder for one cluster's settings.
type workerResourceBuilder struct {
	config KubernetesConfig
}

var _ WorkerResourceBuilder = (*workerResourceBuilder)(nil)

// NewWorkerResourceBuilder creates the WorkerResourceBuilder that renders workers for config's
// namespace, ingress domain and certificate.
func NewWorkerResourceBuilder(config KubernetesConfig) WorkerResourceBuilder {
	return &workerResourceBuilder{config: config}
}

// BuildConfig clones template into one worker's Config.
//
// The clone keeps the template's image, resources, priority class and env, and overrides
// what makes the worker its own: its name and assignee (the executor copies assignee into the
// pod label agent.benjamin-borbe.de/assignee, which is what the egress NetworkPolicy selects
// on), its public URL and card name, and the Secret it reads its credentials from.
//
// The routing fields are neutralised. TaskType, TaskTypes and Trigger are how the executor
// sends vault tasks to a Config by assignee, so a worker that inherited them would start
// receiving the template's vault tasks on top of its own conversation.
//
// TaskType cannot simply be cleared: its JSON tag has no omitempty, so an empty value is sent
// as "" and the CRD rejects it (`spec.taskType` must match ^[a-z0-9-]+$). It is set to the
// worker's own name instead — a valid value no vault task carries, so nothing routes on it.
func (b *workerResourceBuilder) BuildConfig(
	template agentv1.Config,
	workerID WorkerID,
	task TaskName,
) *agentv1.Config {
	name := containerName(workerID)
	spec := template.Spec.DeepCopy()
	spec.Assignee = name
	spec.SecretName = name
	// Deprecated in favour of TaskTypes, but still read by the executor's router, so a
	// clone that kept it would still route vault tasks to the worker.
	spec.TaskType = name //nolint:staticcheck // neutralising a deprecated-but-live routing field
	spec.TaskTypes = nil
	spec.Trigger = nil
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["A2A_PUBLIC_URL"] = b.A2AURL(name)
	spec.Env["A2A_AGENT_NAME"] = b.config.AgentNameBase + "-" + string(workerID)
	return &agentv1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   b.config.Namespace,
			Labels:      workerLabels(workerID),
			Annotations: map[string]string{taskAnnotation: string(task)},
		},
		Spec: *spec,
	}
}

// BuildSecret is the worker's credential Secret: every key of the template's Secret, with
// INTERACTIVE_AUTH_TOKEN replaced by the worker's own token. The executor mounts it as
// envFrom, so each key becomes an environment variable — and replacing the token key is what
// keeps two workers from sharing a bearer.
//
// The values travel from one Secret to another inside this call and are never logged.
func (b *workerResourceBuilder) BuildSecret(
	owner agentv1.Config,
	template corev1.Secret,
	token string,
) *corev1.Secret {
	data := make(map[string][]byte, len(template.Data)+1)
	for key, value := range template.Data {
		data[key] = value
	}
	data[tokenEnvName] = []byte(token)
	return &corev1.Secret{
		ObjectMeta: b.childMeta(owner),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// BuildService is the ClusterIP Service in front of the worker pod. The selector is the pod's
// `app` label, which the executor's StatefulSet sets to the Config's name.
func (b *workerResourceBuilder) BuildService(owner agentv1.Config) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: b.childMeta(owner),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": owner.Name},
			Ports: []corev1.ServicePort{{
				Name:       kubernetesWorkerPortName,
				Port:       kubernetesWorkerPort,
				TargetPort: intstr.FromString(kubernetesWorkerPortName),
			}},
		},
	}
}

// BuildIngress exposes the worker over TLS on its own host, so the manager on the operator's
// Mac can reach it. The annotations are the cluster's house pair: without them the router
// lands on the plaintext entrypoint and the bearer token travels in the clear.
func (b *workerResourceBuilder) BuildIngress(owner agentv1.Config) *networkingv1.Ingress {
	meta := b.childMeta(owner)
	meta.Annotations["traefik.ingress.kubernetes.io/router.entrypoints"] = "websecure"
	meta.Annotations["traefik.ingress.kubernetes.io/router.tls"] = "true"
	pathType := networkingv1.PathTypePrefix
	ingressClassName := b.config.IngressClassName
	host := b.host(owner.Name)
	return &networkingv1.Ingress{
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: owner.Name,
									Port: networkingv1.ServiceBackendPort{
										Name: kubernetesWorkerPortName,
									},
								},
							},
						}},
					},
				},
			}},
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{host},
				SecretName: b.config.TLSSecretName,
			}},
		},
	}
}

// BuildNetworkPolicy confines the worker pod's egress to DNS plus the configured destinations.
//
// It is one policy per worker because no static policy can select the workers alone: the
// executor puts no Config label on the pod, so the only pod label a worker owns is
// agent.benjamin-borbe.de/assignee=<its Config name>, and a selector cannot prefix-match.
// The policy declares only the Egress type, so the Ingress the manager reaches the worker
// through stays open.
func (b *workerResourceBuilder) BuildNetworkPolicy(
	owner agentv1.Config,
) *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dns := intstr.FromInt32(dnsPort)
	rules := make([]networkingv1.NetworkPolicyEgressRule, 0, 1+len(b.config.EgressAllow))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{namespacePeer(dnsNamespace)},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dns},
			{Protocol: &tcp, Port: &dns},
		},
	})
	for _, destination := range b.config.EgressAllow {
		port := intstr.FromInt32(destination.Port)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{namespacePeer(destination.Namespace)},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: b.childMeta(owner),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{assigneePodLabel: owner.Spec.Assignee},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}
}

// namespacePeer selects every pod in namespace, by the name label Kubernetes stamps on each
// namespace itself.
func namespacePeer(namespace string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{namespaceNameLabel: namespace},
		},
	}
}

// A2AURL is the address a worker advertises: its own Ingress host, over TLS. BuildConfig
// writes the same value into A2A_PUBLIC_URL, so the card and list cannot disagree.
func (b *workerResourceBuilder) A2AURL(name string) string {
	return "https://" + b.host(name) + "/a2a"
}

// host is a worker's Ingress host.
func (b *workerResourceBuilder) host(name string) string {
	return name + "." + b.config.IngressDomain
}

// childMeta is the metadata every object a worker owns carries: its Config's name, labels
// and task annotation, and the Config as its controlling owner, so deleting the Config
// garbage-collects it. The maps are fresh copies, so a caller may add to them.
func (b *workerResourceBuilder) childMeta(owner agentv1.Config) metav1.ObjectMeta {
	labels := make(map[string]string, len(owner.Labels))
	for key, value := range owner.Labels {
		labels[key] = value
	}
	return metav1.ObjectMeta{
		Name:        owner.Name,
		Namespace:   b.config.Namespace,
		Labels:      labels,
		Annotations: map[string]string{taskAnnotation: owner.Annotations[taskAnnotation]},
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(&owner, agentv1.SchemeGroupVersion.WithKind("Config")),
		},
	}
}

// workerLabels are the labels every worker object carries. They hold no credential.
func workerLabels(workerID WorkerID) map[string]string {
	return map[string]string{
		controllerLabel: controllerLabelValue,
		workerIDLabel:   string(workerID),
	}
}
