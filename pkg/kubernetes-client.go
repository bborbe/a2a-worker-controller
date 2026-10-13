// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"

	agentv1 "github.com/bborbe/agent-task-executor/k8s/apis/agent.benjamin-borbe.de/v1"
	"github.com/bborbe/agent-task-executor/k8s/client/clientset/versioned"
	"github.com/bborbe/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// kubernetesClient is the KubernetesClient backed by client-go. Config is the agent
// executor's own custom resource, so it goes through that repo's generated typed clientset
// rather than the dynamic client: every field is a checked Go field, never a string lookup.
// Every call is scoped to one namespace.
type kubernetesClient struct {
	agents    versioned.Interface
	clientset kubernetes.Interface
	namespace string
}

var _ KubernetesClient = (*kubernetesClient)(nil)

// NewInClusterKubernetesClient creates the KubernetesClient from the pod's own service
// account. It is the only constructor production uses: the controller runs in the cluster it
// manages, so it needs no kubeconfig and holds no credential beyond its service account.
func NewInClusterKubernetesClient(ctx context.Context, namespace string) (KubernetesClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(ctx, err, "load in-cluster kubernetes config failed")
	}
	agents, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create agent clientset failed")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create clientset failed")
	}
	return &kubernetesClient{agents: agents, clientset: clientset, namespace: namespace}, nil
}

// GetConfig returns the named Config.
func (c *kubernetesClient) GetConfig(ctx context.Context, name string) (*agentv1.Config, error) {
	config, err := c.agents.AgentV1().Configs(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get config %s failed", name)
	}
	return config, nil
}

// ListConfigs returns every Config matching labelSelector.
func (c *kubernetesClient) ListConfigs(
	ctx context.Context,
	labelSelector string,
) ([]agentv1.Config, error) {
	list, err := c.agents.AgentV1().
		Configs(c.namespace).
		List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "list configs %s failed", labelSelector)
	}
	return list.Items, nil
}

// CreateConfig creates config and returns it as stored, carrying its uid.
func (c *kubernetesClient) CreateConfig(
	ctx context.Context,
	config *agentv1.Config,
) (*agentv1.Config, error) {
	created, err := c.agents.AgentV1().
		Configs(c.namespace).
		Create(ctx, config, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "create config %s failed", config.Name)
	}
	return created, nil
}

// DeleteConfig deletes the named Config with background propagation, so the garbage collector
// removes its owned objects — the executor's StatefulSet and this backend's Secret, Service
// and Ingress — without the call waiting for them.
func (c *kubernetesClient) DeleteConfig(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationBackground
	if err := c.agents.AgentV1().Configs(c.namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil {
		return errors.Wrapf(ctx, err, "delete config %s failed", name)
	}
	return nil
}

// GetSecret returns the named Secret. The caller copies its data and must never log it.
func (c *kubernetesClient) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	secret, err := c.clientset.CoreV1().Secrets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get secret %s failed", name)
	}
	return secret, nil
}

// CreateSecret creates secret. The error names the Secret, never its data.
func (c *kubernetesClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	if _, err := c.clientset.CoreV1().
		Secrets(c.namespace).
		Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create secret %s failed", secret.Name)
	}
	return nil
}

// CreateService creates service.
func (c *kubernetesClient) CreateService(ctx context.Context, service *corev1.Service) error {
	if _, err := c.clientset.CoreV1().
		Services(c.namespace).
		Create(ctx, service, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create service %s failed", service.Name)
	}
	return nil
}

// CreateIngress creates ingress.
func (c *kubernetesClient) CreateIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	if _, err := c.clientset.NetworkingV1().
		Ingresses(c.namespace).
		Create(ctx, ingress, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create ingress %s failed", ingress.Name)
	}
	return nil
}
