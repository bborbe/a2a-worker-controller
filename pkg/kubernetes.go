// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"sync"

	"github.com/bborbe/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ConfigGroupVersionResource addresses the agent Config custom resource. A Config of
// `type: service` is what the agent executor reconciles into a long-running StatefulSet, so
// writing one is how this backend starts a worker: the controller never builds a pod itself.
var ConfigGroupVersionResource = schema.GroupVersionResource{
	Group:    "agent.benjamin-borbe.de",
	Version:  "v1",
	Resource: "configs",
}

// configKind and configAPIVersion name the Config for the owner references the worker's
// Service, Ingress and Secret carry.
const (
	configKind       = "Config"
	configAPIVersion = "agent.benjamin-borbe.de/v1"
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

// The template fields the backend removes from every worker Config.
//
// trigger and taskTypes are how the executor routes vault tasks to a Config by assignee. A
// worker inherited them from its template would start receiving the template's vault tasks
// on top of its own conversation, so they are removed rather than copied.
var templateFieldsDropped = []string{"trigger", "taskTypes"}

// The defaults for the k8s backend's settings.
const (
	// DefaultKubernetesTemplateConfig is the Config every worker is cloned from. Cloning a
	// live Config keeps the image pin, resources and priority class in the one place the chart
	// already maintains them, so a worker never runs an image the shared pod does not.
	DefaultKubernetesTemplateConfig = "claude-interactive"
	// DefaultKubernetesIngressClassName is the ingress class every worker Ingress uses.
	DefaultKubernetesIngressClassName = "traefik"
)

// KubernetesConfig holds the k8s backend's settings. It carries no credential: a worker's
// shared credentials are copied from its template's Secret inside the cluster, and its own
// token is minted per worker and returned once.
type KubernetesConfig struct {
	// Namespace is where every worker Config, Secret, Service and Ingress is written.
	Namespace string
	// TemplateConfig names the Config every worker is cloned from.
	TemplateConfig string
	// AgentNameBase prefixes the name each worker's Agent Card advertises.
	AgentNameBase string
	// IngressDomain is the parent domain of every worker host, so a worker named
	// a2a-worker-<id> is served at https://a2a-worker-<id>.<IngressDomain>/a2a.
	IngressDomain string
	// IngressClassName is the ingress class every worker Ingress uses.
	IngressClassName string
	// TLSSecretName names the wildcard certificate Secret covering IngressDomain.
	TLSSecretName string
}

// DefaultKubernetesConfig is the k8s backend's configuration before the deployment-specific
// values (namespace, domain, certificate) are set.
func DefaultKubernetesConfig() KubernetesConfig {
	return KubernetesConfig{
		TemplateConfig:   DefaultKubernetesTemplateConfig,
		AgentNameBase:    DefaultAgentNameBase,
		IngressClassName: DefaultKubernetesIngressClassName,
	}
}

//counterfeiter:generate -o ../mocks/pkg-kubernetes-client.go --fake-name PkgKubernetesClient . KubernetesClient

// KubernetesClient is the k8s backend's only door to the cluster. It is injected, so the
// backend reads no global and a test drives it with a fake — no cluster required. Every call
// is scoped to the one namespace the client was built for.
type KubernetesClient interface {
	// GetConfig returns the named Config.
	GetConfig(ctx context.Context, name string) (*unstructured.Unstructured, error)
	// ListConfigs returns every Config matching labelSelector.
	ListConfigs(ctx context.Context, labelSelector string) ([]unstructured.Unstructured, error)
	// CreateConfig creates config and returns it as stored, carrying its uid.
	CreateConfig(
		ctx context.Context,
		config *unstructured.Unstructured,
	) (*unstructured.Unstructured, error)
	// DeleteConfig deletes the named Config. Its owned objects are garbage-collected.
	DeleteConfig(ctx context.Context, name string) error
	// GetSecret returns the named Secret.
	GetSecret(ctx context.Context, name string) (*corev1.Secret, error)
	// CreateSecret creates secret.
	CreateSecret(ctx context.Context, secret *corev1.Secret) error
	// CreateService creates service.
	CreateService(ctx context.Context, service *corev1.Service) error
	// CreateIngress creates ingress.
	CreateIngress(ctx context.Context, ingress *networkingv1.Ingress) error
}

// kubernetesBackend runs each worker as a per-task pod in the cluster. It writes one
// `type: service` Config per worker, which the agent executor reconciles into a StatefulSet,
// plus the worker's own token Secret, Service and Ingress.
//
// It holds no worker state between calls. Every list reads the Configs the controller label
// marks, so a controller restart loses nothing.
//
// Deletion is a single call by design: the Secret, Service and Ingress carry the Config as
// their owner, and the executor's StatefulSet does too, so deleting the Config lets the
// garbage collector remove every object create made.
type kubernetesBackend struct {
	client KubernetesClient
	config KubernetesConfig
	// mutex serializes Create, because its dedup read happens before the writes that act on
	// it, so two overlapping creates for one task could otherwise start two workers. It closes
	// that race within this process only.
	mutex sync.Mutex
}

var _ WorkerBackend = (*kubernetesBackend)(nil)

// NewKubernetesBackend creates the WorkerBackend that runs each worker as a per-task pod.
//
// client is the backend's only door to the cluster and is injected, so a test drives the
// backend without one. config carries the namespace, the template Config and the ingress
// settings every worker is exposed through.
func NewKubernetesBackend(client KubernetesClient, config KubernetesConfig) WorkerBackend {
	return &kubernetesBackend{client: client, config: config}
}

// Create starts one worker pod for the requested task and returns it with its own freshly
// minted bearer token.
//
// A task that already has a worker returns that worker rather than starting a second. Its
// token is not returned — the controller kept no copy of it — so that reply carries the
// worker and an empty token, the same contract the docker backend keeps.
func (b *kubernetesBackend) Create(
	ctx context.Context,
	req CreateRequest,
) (*CreateResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	configs, err := b.listConfigs(ctx)
	if err != nil {
		return nil, err
	}
	if existing := findConfigByTask(configs, req.Task); existing != nil {
		return &CreateResponse{Worker: b.workerFor(*existing), Token: ""}, nil
	}

	template, err := b.client.GetConfig(ctx, b.config.TemplateConfig)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get template config %s failed", b.config.TemplateConfig)
	}
	templateSecretName, _, err := unstructured.NestedString(template.Object, "spec", "secretName")
	if err != nil || templateSecretName == "" {
		return nil, errors.Errorf(
			ctx,
			"template config %s names no secret to copy credentials from",
			b.config.TemplateConfig,
		)
	}
	templateSecret, err := b.client.GetSecret(ctx, templateSecretName)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get template secret %s failed", templateSecretName)
	}

	workerID, err := newWorkerID(ctx)
	if err != nil {
		return nil, err
	}
	token, err := newToken(ctx)
	if err != nil {
		return nil, err
	}

	name := containerName(workerID)
	workerConfig, err := buildWorkerConfig(ctx, template, b.config, name, workerID, req.Task)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "build worker config failed")
	}

	// The Config is written first because it is the owner the other three objects point at.
	// Its StatefulSet may reach the kubelet before the Secret exists; the kubelet retries a
	// pod whose envFrom Secret is missing, so the pod starts as soon as the Secret lands.
	created, err := b.client.CreateConfig(ctx, workerConfig)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create worker config failed")
	}
	owner := ownerReferenceFor(created)

	if err := b.createChildren(ctx, name, workerID, req.Task, owner, templateSecret, token); err != nil {
		// Deleting the Config garbage-collects whatever children were already written, so a
		// failed create leaves nothing behind for list to report.
		if deleteErr := b.client.DeleteConfig(ctx, name); deleteErr != nil {
			return nil, errors.Wrapf(
				ctx,
				err,
				"create worker objects failed, and rollback delete of config %s failed too: %v",
				name,
				deleteErr,
			)
		}
		return nil, err
	}

	return &CreateResponse{
		Worker: Worker{
			WorkerID: workerID,
			Task:     req.Task,
			A2AURL:   b.a2aURL(name),
			Backend:  BackendKubernetes,
			State:    WorkerStateRunning,
		},
		Token: token,
	}, nil
}

// createChildren writes the worker's token Secret, Service and Ingress, each owned by the
// worker's Config.
func (b *kubernetesBackend) createChildren(
	ctx context.Context,
	name string,
	workerID WorkerID,
	task TaskName,
	owner metav1.OwnerReference,
	templateSecret *corev1.Secret,
	token string,
) error {
	meta := b.childMeta(name, workerID, task, owner)
	if err := b.client.CreateSecret(ctx, buildWorkerSecret(meta, templateSecret, token)); err != nil {
		return errors.Wrap(ctx, err, "create worker secret failed")
	}
	if err := b.client.CreateService(ctx, buildWorkerService(meta, name)); err != nil {
		return errors.Wrap(ctx, err, "create worker service failed")
	}
	if err := b.client.CreateIngress(ctx, buildWorkerIngress(meta, b.config, name)); err != nil {
		return errors.Wrap(ctx, err, "create worker ingress failed")
	}
	return nil
}

// List returns every worker this controller owns, rebuilt from the labelled Configs on every
// call. A Config without the controller label — the chart's own `claude-interactive` among
// them — is never returned.
func (b *kubernetesBackend) List(ctx context.Context) (*ListResponse, error) {
	configs, err := b.listConfigs(ctx)
	if err != nil {
		return nil, err
	}
	workers := make([]Worker, 0, len(configs))
	for _, config := range configs {
		workers = append(workers, b.workerFor(config))
	}
	return &ListResponse{Workers: workers}, nil
}

// Stop deletes the worker the request names, resolving ownership from the Config's own
// labels. A worker the controller does not own — or one that does not exist — is refused with
// an error and left untouched, so a stop can never reach a Config the chart rendered.
func (b *kubernetesBackend) Stop(ctx context.Context, req StopRequest) (*StopResponse, error) {
	configs, err := b.listConfigs(ctx)
	if err != nil {
		return nil, err
	}
	config := findConfigByWorkerID(configs, req.WorkerID)
	if config == nil {
		return nil, errors.Errorf(ctx, "worker %q is not owned by this controller", req.WorkerID)
	}
	if err := b.client.DeleteConfig(ctx, config.GetName()); err != nil {
		return nil, errors.Wrap(ctx, err, "delete worker config failed")
	}
	worker := b.workerFor(*config)
	worker.State = WorkerStateStopped
	return &StopResponse{Worker: worker}, nil
}

// listConfigs reads every Config carrying the controller label. The selector is the whole
// ownership rule: the API server does the filtering, so no other call here can reach a Config
// this controller did not create.
func (b *kubernetesBackend) listConfigs(ctx context.Context) ([]unstructured.Unstructured, error) {
	configs, err := b.client.ListConfigs(ctx, controllerLabel+"="+controllerLabelValue)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list worker configs failed")
	}
	owned := make([]unstructured.Unstructured, 0, len(configs))
	for _, config := range configs {
		// Re-checked here rather than trusted to the selector, so a client that ignored it
		// still cannot hand a foreign Config to stop.
		if config.GetLabels()[controllerLabel] != controllerLabelValue {
			continue
		}
		if config.GetLabels()[workerIDLabel] == "" ||
			config.GetAnnotations()[taskAnnotation] == "" {
			continue
		}
		owned = append(owned, config)
	}
	return owned, nil
}

// workerFor renders a worker Config as the control API's Worker shape. A Config that exists
// is reported running; its pod's readiness is the worker's own card to answer.
func (b *kubernetesBackend) workerFor(config unstructured.Unstructured) Worker {
	return Worker{
		WorkerID: WorkerID(config.GetLabels()[workerIDLabel]),
		Task:     TaskName(config.GetAnnotations()[taskAnnotation]),
		A2AURL:   b.a2aURL(config.GetName()),
		Backend:  BackendKubernetes,
		State:    WorkerStateRunning,
	}
}

// a2aURL is the address a worker advertises: its own Ingress host, over TLS. buildWorkerConfig
// writes the same value into A2A_PUBLIC_URL, so the card and this function cannot disagree.
func (b *kubernetesBackend) a2aURL(name string) string {
	return "https://" + b.host(name) + "/a2a"
}

// host is a worker's Ingress host.
func (b *kubernetesBackend) host(name string) string {
	return hostFor(b.config, name)
}

func hostFor(config KubernetesConfig, name string) string {
	return name + "." + config.IngressDomain
}

// childMeta is the metadata every object a worker owns carries: the same labels as its
// Config, the task annotation, and the Config as its controlling owner.
func (b *kubernetesBackend) childMeta(
	name string,
	workerID WorkerID,
	task TaskName,
	owner metav1.OwnerReference,
) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       b.config.Namespace,
		Labels:          workerLabels(workerID),
		Annotations:     map[string]string{taskAnnotation: string(task)},
		OwnerReferences: []metav1.OwnerReference{owner},
	}
}

// workerLabels are the labels every worker object carries. They hold no credential.
func workerLabels(workerID WorkerID) map[string]string {
	return map[string]string{
		controllerLabel: controllerLabelValue,
		workerIDLabel:   string(workerID),
	}
}

// buildWorkerConfig clones the template Config into one worker's Config.
//
// The clone keeps the template's image, resources, priority class and env, and overrides
// what makes the worker its own: its name and assignee (the executor copies assignee into the
// pod label agent.benjamin-borbe.de/assignee, which is what the egress NetworkPolicy selects
// on), its public URL and card name, and the Secret it reads its credentials from.
func buildWorkerConfig(
	ctx context.Context,
	template *unstructured.Unstructured,
	config KubernetesConfig,
	name string,
	workerID WorkerID,
	task TaskName,
) (*unstructured.Unstructured, error) {
	spec, found, err := unstructured.NestedMap(template.Object, "spec")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.Errorf(ctx, "template config has no spec")
	}
	for _, field := range templateFieldsDropped {
		delete(spec, field)
	}
	spec["assignee"] = name
	spec["secretName"] = name

	env, _, err := unstructured.NestedStringMap(spec, "env")
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	env["A2A_PUBLIC_URL"] = "https://" + hostFor(config, name) + "/a2a"
	env["A2A_AGENT_NAME"] = config.AgentNameBase + "-" + string(workerID)
	envObject := make(map[string]interface{}, len(env))
	for key, value := range env {
		envObject[key] = value
	}
	spec["env"] = envObject

	worker := &unstructured.Unstructured{Object: map[string]interface{}{}}
	worker.SetAPIVersion(configAPIVersion)
	worker.SetKind(configKind)
	worker.SetName(name)
	worker.SetNamespace(config.Namespace)
	worker.SetLabels(workerLabels(workerID))
	worker.SetAnnotations(map[string]string{taskAnnotation: string(task)})
	if err := unstructured.SetNestedMap(worker.Object, spec, "spec"); err != nil {
		return nil, err
	}
	return worker, nil
}

// buildWorkerSecret is the worker's credential Secret: every key of the template's Secret,
// with INTERACTIVE_AUTH_TOKEN replaced by the worker's own token. The executor mounts it as
// envFrom, so each key becomes an environment variable — and replacing the token key is what
// keeps two workers from sharing a bearer.
//
// The values travel from one Secret to another inside this call and are never logged.
func buildWorkerSecret(
	meta metav1.ObjectMeta,
	template *corev1.Secret,
	token string,
) *corev1.Secret {
	data := make(map[string][]byte, len(template.Data)+1)
	for key, value := range template.Data {
		data[key] = value
	}
	data[tokenEnvName] = []byte(token)
	return &corev1.Secret{
		ObjectMeta: meta,
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// buildWorkerService is the ClusterIP Service in front of the worker pod. The selector is
// the pod's `app` label, which the executor's StatefulSet sets to the Config's name.
func buildWorkerService(meta metav1.ObjectMeta, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Name:       kubernetesWorkerPortName,
				Port:       kubernetesWorkerPort,
				TargetPort: intstr.FromString(kubernetesWorkerPortName),
			}},
		},
	}
}

// buildWorkerIngress exposes the worker over TLS on its own host, so the manager on the
// operator's Mac can reach it. The annotations are the cluster's house pair: without them the
// router lands on the plaintext entrypoint and the bearer token travels in the clear.
func buildWorkerIngress(
	meta metav1.ObjectMeta,
	config KubernetesConfig,
	name string,
) *networkingv1.Ingress {
	meta.Annotations = map[string]string{
		taskAnnotation: meta.Annotations[taskAnnotation],
		"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
		"traefik.ingress.kubernetes.io/router.tls":         "true",
	}
	pathType := networkingv1.PathTypePrefix
	ingressClassName := config.IngressClassName
	host := hostFor(config, name)
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
									Name: name,
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
				SecretName: config.TLSSecretName,
			}},
		},
	}
}

// ownerReferenceFor makes config the controlling owner of a worker object, so deleting the
// Config garbage-collects it.
func ownerReferenceFor(config *unstructured.Unstructured) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: configAPIVersion,
		Kind:       configKind,
		Name:       config.GetName(),
		UID:        config.GetUID(),
		Controller: &controller,
	}
}

// findConfigByTask returns the Config serving task, or nil when none does. It is what makes
// a repeated create a no-op rather than a second worker.
func findConfigByTask(
	configs []unstructured.Unstructured,
	task TaskName,
) *unstructured.Unstructured {
	for index := range configs {
		if configs[index].GetAnnotations()[taskAnnotation] == string(task) {
			return &configs[index]
		}
	}
	return nil
}

// findConfigByWorkerID returns the Config carrying workerID, or nil when none does.
func findConfigByWorkerID(
	configs []unstructured.Unstructured,
	workerID WorkerID,
) *unstructured.Unstructured {
	for index := range configs {
		if configs[index].GetLabels()[workerIDLabel] == string(workerID) {
			return &configs[index]
		}
	}
	return nil
}
