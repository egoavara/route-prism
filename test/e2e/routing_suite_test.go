//go:build routing

/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package e2e — routing suite.
//
// Two suites live under the same Ginkgo test entrypoint and are filtered by
// label:
//
//   check  : devloop smoke (assumes the Tilt-deployed sample in `demo` ns)
//   full   : self-contained regression (G1, G3, G4, G7, G8) provisioning
//            its own per-group namespaces under e2e-*.
//
// Both run against a cluster that already has the route-prism controller
// running (Tilt brings that up). They auto-detect the mesh platform via
// GatewayClass list (cilium / istio / cilium-istio).
//
// Run via Tilt buttons (`check`, `e2e-full`) or directly:
//
//   go test -tags routing ./test/e2e/ -ginkgo.label-filter=check -count=1
//   go test -tags routing ./test/e2e/ -ginkgo.label-filter=full  -count=1
package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

type Platform string

const (
	PlatformCilium      Platform = "cilium"
	PlatformIstio       Platform = "istio"
	PlatformCiliumIstio Platform = "cilium-istio"
)

// IsIstioDataplane returns true when the platform uses Istio's L7 dataplane
// (waypoints) — i.e. either pure istio or cilium-istio combo.
func (p Platform) IsIstioDataplane() bool {
	return p == PlatformIstio || p == PlatformCiliumIstio
}

const (
	// Label applied to every namespace this suite creates so AfterSuite can
	// reliably pick them up for cleanup, and so KEEP=1 inspections are
	// trivially scriptable (kubectl get ns -l route-prism.egoavara.net/e2e=routing).
	NSLabelKey   = "route-prism.egoavara.net/e2e"
	NSLabelValue = "routing"
)

var (
	k8sClient client.Client
	platform  Platform

	// keepNamespaces leaves e2e-* namespaces around after the suite for
	// post-mortem inspection. Set KEEP=1 in env.
	keepNamespaces = os.Getenv("KEEP") == "1"

	// suiteCtx is shared across all specs.
	suiteCtx = context.Background()
)

func TestRouting(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "routing")
}

var _ = BeforeSuite(func() {
	By("registering schemes")
	Expect(routeprismv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(gwv1.Install(scheme.Scheme)).To(Succeed())

	By("building Kubernetes client")
	cfg, err := ctrl.GetConfig()
	Expect(err).NotTo(HaveOccurred(), "could not load kubeconfig — make sure KUBECONFIG points at the kind cluster")
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	By("detecting mesh platform via GatewayClass")
	platform = detectPlatform(suiteCtx)
	fmt.Fprintf(GinkgoWriter, "platform: %s\n", platform)

	// Gomega defaults: most operations are k8s reconcile loops — give them
	// generous time budgets but poll often enough to feel responsive.
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
})

var _ = AfterSuite(func() {
	if keepNamespaces {
		By("KEEP=1 — leaving namespaces for inspection")
		return
	}
	By("deleting namespaces created by this suite")
	var nss = listSuiteNamespaces(suiteCtx)
	for _, n := range nss {
		ns := n
		// best-effort, async — don't block the report.
		_ = k8sClient.Delete(suiteCtx, &ns, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}
})
