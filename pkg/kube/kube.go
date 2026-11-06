// Package kube builds Kubernetes clients for a kubeconfig + context pair.
package kube

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config returns the REST config for the given kubeconfig path and context.
// Empty values fall back to the default loading rules / current context.
func Config(kubeconfig, context string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (context %q): %w", context, err)
	}
	cfg.UserAgent = "hopcharlie-migration-engine"
	return cfg, nil
}

// Dynamic returns a dynamic client for the given kubeconfig path and context.
func Dynamic(kubeconfig, context string) (dynamic.Interface, error) {
	cfg, err := Config(kubeconfig, context)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

// Clients returns a typed and a dynamic client for the same context.
func Clients(kubeconfig, context string) (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := Config(kubeconfig, context)
	if err != nil {
		return nil, nil, err
	}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return typed, dyn, nil
}
