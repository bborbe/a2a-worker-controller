// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"sync"

	agentv1 "github.com/bborbe/agent-task-executor/k8s/apis/agent.benjamin-borbe.de/v1"
	"github.com/bborbe/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

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
	// EgressAllow lists the in-cluster destinations each worker's NetworkPolicy allows, on top
	// of DNS. Everything else — the public internet included — is denied.
	EgressAllow []EgressDestination
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
// backend reads no global and a test drives it with a mock — no cluster required. Every call
// is scoped to the one namespace the client was built for.
type KubernetesClient interface {
	// GetConfig returns the named Config.
	GetConfig(ctx context.Context, name string) (*agentv1.Config, error)
	// ListConfigs returns every Config matching labelSelector.
	ListConfigs(ctx context.Context, labelSelector string) ([]agentv1.Config, error)
	// CreateConfig creates config and returns it as stored, carrying its uid.
	CreateConfig(ctx context.Context, config *agentv1.Config) (*agentv1.Config, error)
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
	// CreateNetworkPolicy creates policy.
	CreateNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) error
}

// kubernetesBackend runs each worker as a per-task pod in the cluster. It writes one
// `type: service` Config per worker, which the agent executor reconciles into a StatefulSet,
// plus the worker's own token Secret, Service and Ingress. What those objects contain is the
// builder's business; this type only sequences the reads and writes around them.
//
// It holds no worker state between calls. Every list reads the Configs the controller label
// marks, so a controller restart loses nothing.
//
// Deletion is a single call by design: the Secret, Service and Ingress carry the Config as
// their owner, and the executor's StatefulSet does too, so deleting the Config lets the
// garbage collector remove every object create made.
type kubernetesBackend struct {
	client         KubernetesClient
	builder        WorkerResourceBuilder
	templateConfig string
	// mutex serializes Create, because its dedup read happens before the writes that act on
	// it, so two overlapping creates for one task could otherwise start two workers. It closes
	// that race within this process only.
	mutex sync.Mutex
}

var _ WorkerBackend = (*kubernetesBackend)(nil)

// NewKubernetesBackend creates the WorkerBackend that runs each worker as a per-task pod.
//
// client is the backend's only door to the cluster and builder renders every object a worker
// is made of; both are injected, so a test drives the backend without a cluster.
// templateConfig names the Config every worker is cloned from.
func NewKubernetesBackend(
	client KubernetesClient,
	builder WorkerResourceBuilder,
	templateConfig string,
) WorkerBackend {
	return &kubernetesBackend{client: client, builder: builder, templateConfig: templateConfig}
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
	if existing := configs.byTask(req.Task); existing != nil {
		return &CreateResponse{Worker: b.workerFor(*existing), Token: ""}, nil
	}

	template, err := b.client.GetConfig(ctx, b.templateConfig)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get template config %s failed", b.templateConfig)
	}
	if template.Spec.SecretName == "" {
		return nil, errors.Errorf(
			ctx,
			"template config %s names no secret to copy credentials from",
			b.templateConfig,
		)
	}
	templateSecret, err := b.client.GetSecret(ctx, template.Spec.SecretName)
	if err != nil {
		return nil, errors.Wrapf(
			ctx,
			err,
			"get template secret %s failed",
			template.Spec.SecretName,
		)
	}

	workerID, err := newWorkerID(ctx)
	if err != nil {
		return nil, err
	}
	token, err := newToken(ctx)
	if err != nil {
		return nil, err
	}

	// The Config is written first because it is the owner the other three objects point at.
	// Its StatefulSet may reach the kubelet before the Secret exists; the kubelet retries a
	// pod whose envFrom Secret is missing, so the pod starts as soon as the Secret lands.
	created, err := b.client.CreateConfig(
		ctx,
		b.builder.BuildConfig(*template, workerID, req.Task),
	)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create worker config failed")
	}

	if err := b.createChildren(ctx, *created, *templateSecret, token); err != nil {
		// Deleting the Config garbage-collects whatever children were already written, so a
		// failed create leaves nothing behind for list to report.
		if deleteErr := b.client.DeleteConfig(ctx, created.Name); deleteErr != nil {
			return nil, errors.Wrapf(
				ctx,
				err,
				"create worker objects failed, and rollback delete of config %s failed too: %v",
				created.Name,
				deleteErr,
			)
		}
		return nil, err
	}

	return &CreateResponse{Worker: b.workerFor(*created), Token: token}, nil
}

// createChildren writes the worker's NetworkPolicy, token Secret, Service and Ingress, each
// owned by owner.
//
// The NetworkPolicy is written before the Secret on purpose. The pod cannot start until its
// envFrom Secret exists, so a policy that lands first is in force before the worker's first
// connection — there is no window in which a worker runs with unrestricted egress.
func (b *kubernetesBackend) createChildren(
	ctx context.Context,
	owner agentv1.Config,
	templateSecret corev1.Secret,
	token string,
) error {
	if err := b.client.CreateNetworkPolicy(ctx, b.builder.BuildNetworkPolicy(owner)); err != nil {
		return errors.Wrap(ctx, err, "create worker network policy failed")
	}
	if err := b.client.CreateSecret(ctx, b.builder.BuildSecret(owner, templateSecret, token)); err != nil {
		return errors.Wrap(ctx, err, "create worker secret failed")
	}
	if err := b.client.CreateService(ctx, b.builder.BuildService(owner)); err != nil {
		return errors.Wrap(ctx, err, "create worker service failed")
	}
	if err := b.client.CreateIngress(ctx, b.builder.BuildIngress(owner)); err != nil {
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
	config := configs.byWorkerID(req.WorkerID)
	if config == nil {
		return nil, errors.Errorf(ctx, "worker %q is not owned by this controller", req.WorkerID)
	}
	if err := b.client.DeleteConfig(ctx, config.Name); err != nil {
		return nil, errors.Wrap(ctx, err, "delete worker config failed")
	}
	worker := b.workerFor(*config)
	worker.State = WorkerStateStopped
	return &StopResponse{Worker: worker}, nil
}

// listConfigs reads every Config carrying the controller label. The selector is the whole
// ownership rule: the API server does the filtering, so no other call here can reach a Config
// this controller did not create.
func (b *kubernetesBackend) listConfigs(ctx context.Context) (workerConfigs, error) {
	configs, err := b.client.ListConfigs(ctx, controllerLabel+"="+controllerLabelValue)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list worker configs failed")
	}
	owned := make(workerConfigs, 0, len(configs))
	for _, config := range configs {
		// Re-checked here rather than trusted to the selector, so a client that ignored it
		// still cannot hand a foreign Config to stop.
		if config.Labels[controllerLabel] != controllerLabelValue {
			continue
		}
		if config.Labels[workerIDLabel] == "" || config.Annotations[taskAnnotation] == "" {
			continue
		}
		owned = append(owned, config)
	}
	return owned, nil
}

// workerFor renders a worker Config as the control API's Worker shape. A Config that exists
// is reported running; its pod's readiness is the worker's own card to answer.
func (b *kubernetesBackend) workerFor(config agentv1.Config) Worker {
	return Worker{
		WorkerID: WorkerID(config.Labels[workerIDLabel]),
		Task:     TaskName(config.Annotations[taskAnnotation]),
		A2AURL:   b.builder.A2AURL(config.Name),
		Backend:  BackendKubernetes,
		State:    WorkerStateRunning,
	}
}

// workerConfigs is the set of Configs this controller owns, as listConfigs read it.
type workerConfigs []agentv1.Config

// byTask returns the Config serving task, or nil when none does. It is what makes a repeated
// create a no-op rather than a second worker.
func (c workerConfigs) byTask(task TaskName) *agentv1.Config {
	for index := range c {
		if c[index].Annotations[taskAnnotation] == string(task) {
			return &c[index]
		}
	}
	return nil
}

// byWorkerID returns the Config carrying workerID, or nil when none does.
func (c workerConfigs) byWorkerID(workerID WorkerID) *agentv1.Config {
	for index := range c {
		if c[index].Labels[workerIDLabel] == string(workerID) {
			return &c[index]
		}
	}
	return nil
}
