/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package verify implements the `route-prism verify` subcommand: an
// interactive, kubeconfig-driven check that the user's cluster has a
// GAMMA-supporting mesh installed and that it actually accepts an
// HTTPRoute with a Service parentRef.
package verify

import (
	"fmt"
	"sort"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Context describes one entry from the user's kubeconfig.
type Context struct {
	Name      string
	Cluster   string
	User      string
	Namespace string
	IsCurrent bool
}

// LoadContexts reads the merged kubeconfig (default precedence: --kubeconfig
// flag → KUBECONFIG env → ~/.kube/config) and returns a stable, alphabetised
// list of contexts.
func LoadContexts(explicitPath string) ([]Context, *clientcmdapi.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicitPath != "" {
		rules.ExplicitPath = explicitPath
	}
	cfg, err := rules.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if len(cfg.Contexts) == 0 {
		return nil, cfg, fmt.Errorf("no contexts found in kubeconfig")
	}

	out := make([]Context, 0, len(cfg.Contexts))
	for name, ctx := range cfg.Contexts {
		out = append(out, Context{
			Name:      name,
			Cluster:   ctx.Cluster,
			User:      ctx.AuthInfo,
			Namespace: ctx.Namespace,
			IsCurrent: name == cfg.CurrentContext,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Current context first, then alphabetical.
		if out[i].IsCurrent != out[j].IsCurrent {
			return out[i].IsCurrent
		}
		return out[i].Name < out[j].Name
	})
	return out, cfg, nil
}

// RestConfigFor builds a rest.Config bound to the named context.
func RestConfigFor(explicitPath, contextName string) (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicitPath != "" {
		rules.ExplicitPath = explicitPath
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build rest config for context %q: %w", contextName, err)
	}
	return rc, nil
}
