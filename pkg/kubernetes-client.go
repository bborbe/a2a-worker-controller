// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"

	"github.com/bborbe/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// clientGoKubernetesClient is the KubernetesClient backed by client-go. Config is a custom
// resource, so it goes through the dynamic client; the built-in kinds go through the typed
// clientset. Every call is scoped to one namespace.
type clientGoKubernetesClient struct {
	dynamic   dynamic.Interface
	clientset kubernetes.Interface
	namespace string
}

var _ KubernetesClient = (*clientGoKubernetesClient)(nil)

// NewInClusterKubernetesClient creates the KubernetesClient from the pod's own service
// account. It is the only constructor production uses: the controller runs in the cluster it
// manages, so it needs no kubeconfig and holds no credential beyond its service account.
func NewInClusterKubernetesClient(ctx context.Context, namespace string) (KubernetesClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(ctx, err, "load in-cluster kubernetes config failed")
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create dynamic client failed")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "create clientset failed")
	}
	return &clientGoKubernetesClient{
		dynamic:   dynamicClient,
		clientset: clientset,
		namespace: namespace,
	}, nil
}

func (c *clientGoKubernetesClient) configs() dynamic.ResourceInterface {
	return c.dynamic.Resource(ConfigGroupVersionResource).Namespace(c.namespace)
}

// GetConfig returns the named Config.
func (c *clientGoKubernetesClient) GetConfig(
	ctx context.Context,
	name string,
) (*unstructured.Unstructured, error) {
	config, err := c.configs().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get config %s failed", name)
	}
	return config, nil
}

// ListConfigs returns every Config matching labelSelector.
func (c *clientGoKubernetesClient) ListConfigs(
	ctx context.Context,
	labelSelector string,
) ([]unstructured.Unstructured, error) {
	list, err := c.configs().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "list configs %s failed", labelSelector)
	}
	return list.Items, nil
}

// CreateConfig creates config and returns it as stored, carrying its uid.
func (c *clientGoKubernetesClient) CreateConfig(
	ctx context.Context,
	config *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	created, err := c.configs().Create(ctx, config, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "create config %s failed", config.GetName())
	}
	return created, nil
}

// DeleteConfig deletes the named Config with background propagation, so the garbage collector
// removes its owned objects — the executor's StatefulSet and this backend's Secret, Service
// and Ingress — without the call waiting for them.
func (c *clientGoKubernetesClient) DeleteConfig(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationBackground
	if err := c.configs().Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil {
		return errors.Wrapf(ctx, err, "delete config %s failed", name)
	}
	return nil
}

// GetSecret returns the named Secret. The caller copies its data and must never log it.
func (c *clientGoKubernetesClient) GetSecret(
	ctx context.Context,
	name string,
) (*corev1.Secret, error) {
	secret, err := c.clientset.CoreV1().Secrets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "get secret %s failed", name)
	}
	return secret, nil
}

// CreateSecret creates secret. The error names the Secret, never its data.
func (c *clientGoKubernetesClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	if _, err := c.clientset.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create secret %s failed", secret.Name)
	}
	return nil
}

// CreateService creates service.
func (c *clientGoKubernetesClient) CreateService(
	ctx context.Context,
	service *corev1.Service,
) error {
	if _, err := c.clientset.CoreV1().Services(c.namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create service %s failed", service.Name)
	}
	return nil
}

// CreateIngress creates ingress.
func (c *clientGoKubernetesClient) CreateIngress(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) error {
	if _, err := c.clientset.NetworkingV1().Ingresses(c.namespace).Create(ctx, ingress, metav1.CreateOptions{}); err != nil {
		return errors.Wrapf(ctx, err, "create ingress %s failed", ingress.Name)
	}
	return nil
}
