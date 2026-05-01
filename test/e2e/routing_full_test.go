//go:build routing

/*
Copyright 2026.
*/

// Full regression suite — equivalent of the legacy hack/e2e-full.sh.
//
// Each scenario group provisions its own dedicated namespace (under e2e-*),
// creates its own workloads / Services / CRs, runs assertions, then relies
// on AfterSuite to tear down. Nothing depends on the devloop sample.
//
//   G1  ContextRoute standalone — Baggage-based fork in the mesh
//   G3  EdgeTransformation cookie fallback when the cookie value is unknown
//   G4  Multiple variants — deterministic rule order + per-variant fork
//   G7  CR/ET status conditions — Ready/Reason transitions
//   G8  appProtocol gate — Service without appProtocol on its port emits a
//       MissingAppProtocol Event (Ready stays True; warning, not failure)

package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

var _ = Describe("regression", Label("full"), func() {

	// ── G1: ContextRoute standalone — mesh-side Baggage fork ────────
	Context("G1 ContextRoute standalone (Baggage fork in mesh)", Label("g1"), Ordered, func() {
		const ns = "e2e-g1"
		host := "web." + ns + ".svc.cluster.local"
		var probes []ProbeResult

		BeforeAll(func() {
			setupNamespace(suiteCtx, ns)
			deployWorkload(suiteCtx, ns, "web", "stable", AppProtoHTTP)
			deployWorkload(suiteCtx, ns, "web-canary", "canary", AppProtoHTTP)
			applyContextRoute(suiteCtx, ns, "route", "web", map[string]string{"app": "web"})

			By("CR reaches Ready=True/Reconciled")
			expectReady(suiteCtx, ns, &routeprismv1alpha1.ContextRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: ns},
			}, metav1.ConditionTrue, "Reconciled")

			By("CR HTTPRoute exists")
			Eventually(func(g Gomega) {
				var hr gwv1.HTTPRoute
				g.Expect(k8sClient.Get(suiteCtx,
					client.ObjectKey{Namespace: ns, Name: "route-prism-cr-web"}, &hr)).To(Succeed())
			}).Should(Succeed())

			// Default RoutingKey for a CR with no override: "<ns>.<target>".
			rk := ns + ".web"
			probes = runProbes(ns, []Probe{
				{Tag: "P1", URL: "http://" + host + "/"},
				{Tag: "P2", URL: "http://" + host + "/", Headers: []string{"Baggage: " + rk + "=web-canary"}},
				{Tag: "P3", URL: "http://" + host + "/", Headers: []string{"Baggage: " + rk + "=does-not-exist"}},
			})
		})

		It("no Baggage → catch-all rule → stable", func() {
			expectVariant(probes[0], "web", fmt.Sprintf("GET http://%s/  (no headers)", host))
		})
		It("Baggage selecting web-canary → variant rule fires → canary", func() {
			expectVariant(probes[1], "web-canary",
				fmt.Sprintf("GET http://%s/  + Baggage: %s.web=web-canary", host, ns))
		})
		It("Baggage with unknown variant → catch-all → stable", func() {
			expectVariant(probes[2], "web",
				fmt.Sprintf("GET http://%s/  + Baggage: %s.web=does-not-exist", host, ns))
		})
	})

	// ── G3: EdgeTransformation cookie fallback ─────────────────────────
	Context("G3 EdgeTransformation cookie fallback (unknown cookie value)", Label("g3"), Ordered, func() {
		const ns = "e2e-g3"
		host := "web." + ns + ".svc.cluster.local"
		var probes []ProbeResult

		BeforeAll(func() {
			setupNamespace(suiteCtx, ns)
			deployWorkload(suiteCtx, ns, "web", "stable", AppProtoHTTP)
			deployWorkload(suiteCtx, ns, "web-canary", "canary", AppProtoHTTP)
			applyContextRoute(suiteCtx, ns, "route", "web", map[string]string{"app": "web"})
			applyEdgeTransformation(suiteCtx, ns, "edge", "web", "router", "x-route-prism")

			By("translator Deployment becomes Ready")
			Eventually(func(g Gomega) {
				deployReady(g, ns, "route-prism-translator-edge")
			}).Should(Succeed())

			// Cookie value uses the new multi-key syntax `<routingKey>:<variant>`.
			// Default RoutingKey for this CR (no override) = "<ns>.<target>".
			rk := ns + ".web"
			probes = runProbes(ns, []Probe{
				{Tag: "C1", URL: "http://" + host + "/", Headers: []string{"Cookie: x-route-prism=" + rk + ":does-not-exist"}},
				{Tag: "C2", URL: "http://" + host + "/", Headers: []string{"Cookie: x-route-prism=" + rk + ":web-canary"}},
				{Tag: "C3", URL: "http://" + host + "/"},
			})
		})

		It("unknown cookie variant → translator default Pod → stable", func() {
			expectVariant(probes[0], "web",
				fmt.Sprintf("GET http://%s/  + Cookie: x-route-prism=%s.web:does-not-exist", host, ns))
		})
		It("matching variant in cookie → translator forwards to variant → canary", func() {
			expectVariant(probes[1], "web-canary",
				fmt.Sprintf("GET http://%s/  + Cookie: x-route-prism=%s.web:web-canary", host, ns))
		})
		It("missing cookie → translator default → stable", func() {
			expectVariant(probes[2], "web", fmt.Sprintf("GET http://%s/  (no cookie)", host))
		})
	})

	// ── G4: Multiple variants ───────────────────────────────────────
	Context("G4 multiple variants — deterministic per-variant fork", Label("g4"), Ordered, func() {
		const ns = "e2e-g4"
		host := "web." + ns + ".svc.cluster.local"
		var probes []ProbeResult

		BeforeAll(func() {
			setupNamespace(suiteCtx, ns)
			deployWorkload(suiteCtx, ns, "web", "stable", AppProtoHTTP)
			deployWorkload(suiteCtx, ns, "web-v1", "v1", AppProtoHTTP)
			deployWorkload(suiteCtx, ns, "web-v2", "v2", AppProtoHTTP)
			applyContextRoute(suiteCtx, ns, "route", "web", map[string]string{"app": "web"})

			expectReady(suiteCtx, ns, &routeprismv1alpha1.ContextRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: ns},
			}, metav1.ConditionTrue, "Reconciled")

			rk := ns + ".web"
			probes = runProbes(ns, []Probe{
				{Tag: "M1", URL: "http://" + host + "/", Headers: []string{"Baggage: " + rk + "=web-v1"}},
				{Tag: "M2", URL: "http://" + host + "/", Headers: []string{"Baggage: " + rk + "=web-v2"}},
				{Tag: "M3", URL: "http://" + host + "/", Headers: []string{"Baggage: " + rk + "=web-vX"}},
				{Tag: "M4", URL: "http://" + host + "/"},
			})
		})

		It("CR.status.variantServices excludes target and lists discovered variants", func() {
			var cr routeprismv1alpha1.ContextRoute
			Expect(k8sClient.Get(suiteCtx, client.ObjectKey{Namespace: ns, Name: "route"}, &cr)).To(Succeed())
			Expect(cr.Status.VariantServices).To(Equal([]string{"web-v1", "web-v2"}),
				"variantServices=%v", cr.Status.VariantServices)
		})

		It("HTTPRoute rules are emitted in alphabetical order with catch-all last", func() {
			var hr gwv1.HTTPRoute
			Expect(k8sClient.Get(suiteCtx,
				client.ObjectKey{Namespace: ns, Name: "route-prism-cr-web"}, &hr)).To(Succeed())
			var got []string
			for _, r := range hr.Spec.Rules {
				if len(r.BackendRefs) > 0 {
					got = append(got, string(r.BackendRefs[0].Name))
				}
			}
			Expect(got).To(Equal([]string{"web-v1", "web-v2", "web"}),
				"rule order=%v", got)
		})

		It("Baggage selects v1", func() {
			expectVariant(probes[0], "web-v1",
				fmt.Sprintf("GET http://%s/  + Baggage: %s.web=web-v1", host, ns))
		})
		It("Baggage selects v2 (not shadowed by v1 rule)", func() {
			expectVariant(probes[1], "web-v2",
				fmt.Sprintf("GET http://%s/  + Baggage: %s.web=web-v2", host, ns))
		})
		It("Baggage with unknown variant → stable", func() {
			expectVariant(probes[2], "web",
				fmt.Sprintf("GET http://%s/  + Baggage: %s.web=web-vX", host, ns))
		})
		It("no Baggage → stable", func() {
			expectVariant(probes[3], "web", fmt.Sprintf("GET http://%s/  (no headers)", host))
		})
	})

	// ── G7: Status conditions ───────────────────────────────────────
	Context("G7 CR/ET status conditions", Label("g7"), Ordered, func() {
		const ns = "e2e-g7"

		BeforeAll(func() {
			setupNamespace(suiteCtx, ns)
		})

		It("7a: CR pointing to non-existent target → Ready=False/TargetNotFound", func() {
			applyContextRoute(suiteCtx, ns, "cr-missing", "nope",
				map[string]string{"app": "nope"})
			expectReady(suiteCtx, ns, &routeprismv1alpha1.ContextRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "cr-missing", Namespace: ns},
			}, metav1.ConditionFalse, "TargetNotFound")
		})

		It("7b: CR + ET on same target → CR delegates routing", func() {
			deployWorkload(suiteCtx, ns, "web", "stable", AppProtoHTTP)
			deployWorkload(suiteCtx, ns, "web-canary", "canary", AppProtoHTTP)
			applyContextRoute(suiteCtx, ns, "cr-deleg", "web", map[string]string{"app": "web"})
			applyEdgeTransformation(suiteCtx, ns, "et-router", "web", "router", "x-route-prism")
			expectReady(suiteCtx, ns, &routeprismv1alpha1.ContextRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "cr-deleg", Namespace: ns},
			}, metav1.ConditionTrue, "DelegatedToEdgeTransformation")
		})

		It("7c: ET with unimplemented mode (envoy-lua) → Ready=False/ModeNotImplemented", func() {
			applyEdgeTransformation(suiteCtx, ns, "et-lua", "web", "envoy-lua", "x-route-prism")
			expectReady(suiteCtx, ns, &routeprismv1alpha1.EdgeTransformation{
				ObjectMeta: metav1.ObjectMeta{Name: "et-lua", Namespace: ns},
			}, metav1.ConditionFalse, "ModeNotImplemented")
		})
	})

	// ── G8: appProtocol gate ────────────────────────────────────────
	Context("G8 appProtocol gate (warning, not hard failure)", Label("g8"), Ordered, func() {
		const ns = "e2e-g8"

		BeforeAll(func() {
			setupNamespace(suiteCtx, ns)
			deployWorkload(suiteCtx, ns, "web", "stable", AppProtoNone) // <-- no appProtocol
			deployWorkload(suiteCtx, ns, "web-canary", "canary", AppProtoHTTP)
			applyContextRoute(suiteCtx, ns, "cr-noproto", "web", map[string]string{"app": "web"})
			applyEdgeTransformation(suiteCtx, ns, "et-noproto", "web", "router", "x-route-prism")
		})

		It("ContextRoute emits MissingAppProtocol Warning Event", func() {
			expectWarningEvent(suiteCtx, ns, "ContextRoute", "cr-noproto", "MissingAppProtocol")
		})
		It("EdgeTransformation emits MissingAppProtocol Warning Event", func() {
			expectWarningEvent(suiteCtx, ns, "EdgeTransformation", "et-noproto", "MissingAppProtocol")
		})
		It("CR still reaches Ready=True/DelegatedToEdgeTransformation despite missing appProtocol", func() {
			expectReady(suiteCtx, ns, &routeprismv1alpha1.ContextRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "cr-noproto", Namespace: ns},
			}, metav1.ConditionTrue, "DelegatedToEdgeTransformation")
		})
	})
})

// deployReady fails the gomega assertion unless the named Deployment has
// at least one ready replica. Reused across groups that wait on
// controller-provisioned workloads (e.g. the translator).
func deployReady(g Gomega, ns, name string) {
	GinkgoHelper()
	var d appsv1.Deployment
	g.Expect(k8sClient.Get(suiteCtx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
	g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1),
		"Deployment %s/%s not ready (readyReplicas=%d)", ns, name, d.Status.ReadyReplicas)
}
