// Package kube builds Kubernetes clients for a kubeconfig + context pair.
package kube

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Dynamic returns a dynamic client for the given kubeconfig path and context.
// Empty values fall back to the default loading rules / current context.
func Dynamic(kubeconfig, context string) (dynamic.Interface, error) {
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
	return dynamic.NewForConfig(cfg)
}
