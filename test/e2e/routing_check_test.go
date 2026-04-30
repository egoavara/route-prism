//go:build routing

/*
Copyright 2026.
*/

// Devloop smoke check — exercises the 3-tier sample under the `demo`
// namespace that Tilt deploys via test/devloop/sample.yaml.
//
// Topology recap: web → api → db, each tier identical (sample-tier
// forwarder + ContextRoute + smart-mode EdgeTranslation). The forwarder
// propagates the inbound Cookie + Baggage to its NEXT_HOP, so a single
// GET against `web` returns a nested chain JSON with one hop per tier.
//
// We verify the chain by sending a single request to web and parsing the
// nested response — not by hitting each tier independently. That way the
// test exercises mesh routing AND Cookie propagation across hops, and a
// failure at any tier shows up with concrete request/expected/observed
// per hop.
//
// Cookie semantics: every tier's EdgeTranslation reads the same cookie
// name `x-route-prism`. The cookie value is the variant Service name to
// fork to. Only the tier whose variants list contains a Service named
// after the cookie value forks — other tiers fall through to default.

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// chainScenario describes one cookie scenario plus the variant we expect
// at each tier in the chain. Variants are deployment names (web /
// web-canary / api / api-canary / db / db-canary), matching what
// variantOf() returns for any pod.
type chainScenario struct {
	name      string
	cookie    string // empty = no cookie sent
	wantWeb   string
	wantAPI   string
	wantDB    string
	descShort string
}

// Cookie value format (multi-key, '|' separated):
//
//	x-route-prism=<routingKey>:<variant>|<routingKey>:<variant>|...
//
// Each ContextRoute's effective routingKey defaults to "<ns>.<target>",
// so for the demo namespace's three tiers the keys are demo.web /
// demo.api / demo.db. A `.` value means "no fork at this tier".
var chainScenarios = []chainScenario{
	{
		name:      "no-cookie",
		descShort: "no cookie → all tiers stable",
		wantWeb:   "web", wantAPI: "api", wantDB: "db",
	},
	{
		name:      "fork-web",
		cookie:    "x-route-prism=demo.web:web-canary",
		descShort: "fork only web; api+db stay stable",
		wantWeb:   "web-canary", wantAPI: "api", wantDB: "db",
	},
	{
		name:      "fork-api",
		cookie:    "x-route-prism=demo.api:api-canary",
		descShort: "fork only api; web+db stay stable",
		wantWeb:   "web", wantAPI: "api-canary", wantDB: "db",
	},
	{
		name:      "fork-db",
		cookie:    "x-route-prism=demo.db:db-canary",
		descShort: "fork only db; web+api stay stable",
		wantWeb:   "web", wantAPI: "api", wantDB: "db-canary",
	},
	{
		// New capability: a single cookie carries multi-tier directives.
		// Sibling translators each pick out their own routingKey entry,
		// preserve unrelated entries through to baggage propagation.
		name:      "fork-web-and-db",
		cookie:    "x-route-prism=demo.web:web-canary|demo.api:.|demo.db:db-canary",
		descShort: "fork web + db; api explicitly opted out (.)",
		wantWeb:   "web-canary", wantAPI: "api", wantDB: "db-canary",
	},
	{
		name:      "fork-all",
		cookie:    "x-route-prism=demo.web:web-canary|demo.api:api-canary|demo.db:db-canary",
		descShort: "fork all three tiers in a single cookie",
		wantWeb:   "web-canary", wantAPI: "api-canary", wantDB: "db-canary",
	},
}

var _ = Describe("devloop smoke (3-tier chain)", Label("check"), Ordered, func() {
	const ns = "demo"
	const webHost = "web.demo.svc.cluster.local"

	probes := make(map[string][]chainHop, len(chainScenarios))

	BeforeAll(func() {
		By("waiting for every tier's stable + canary + translator Deployments to be Ready")
		expected := []string{
			"web", "web-canary", "route-prism-translator-web-edge",
			"api", "api-canary", "route-prism-translator-api-edge",
			"db", "db-canary", "route-prism-translator-db-edge",
		}
		for _, name := range expected {
			n := name
			Eventually(func(g Gomega) {
				var d appsv1.Deployment
				g.Expect(k8sClient.Get(suiteCtx, client.ObjectKey{Namespace: ns, Name: n}, &d)).To(Succeed())
				g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1))
			}, 3*time.Minute, 2*time.Second).Should(Succeed(), "Deployment %s/%s never became Ready", ns, n)
		}

		// On Istio dataplane: ensure a waypoint Gateway exists in `demo`.
		// sample.yaml already labels the namespace as ambient.
		if platform.IsIstioDataplane() {
			By("ensuring waypoint Gateway in demo (idempotent)")
			gw := &gwv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "waypoint",
					Namespace: ns,
					Labels:    map[string]string{"istio.io/waypoint-for": "service"},
				},
				Spec: gwv1.GatewaySpec{
					GatewayClassName: gwv1.ObjectName("istio-waypoint"),
					Listeners: []gwv1.Listener{{
						Name:     "mesh",
						Port:     gwv1.PortNumber(15008),
						Protocol: gwv1.ProtocolType("HBONE"),
					}},
				},
			}
			createOrPatch(suiteCtx, gw, func(_ client.Object) {})

			var demo corev1.Namespace
			Expect(k8sClient.Get(suiteCtx, client.ObjectKey{Name: ns}, &demo)).To(Succeed())
			if demo.Labels["istio.io/use-waypoint"] != "waypoint" {
				if demo.Labels == nil {
					demo.Labels = map[string]string{}
				}
				demo.Labels["istio.io/use-waypoint"] = "waypoint"
				Expect(k8sClient.Update(suiteCtx, &demo)).To(Succeed())
			}
			Eventually(func(g Gomega) {
				var got gwv1.Gateway
				g.Expect(k8sClient.Get(suiteCtx, client.ObjectKey{Namespace: ns, Name: "waypoint"}, &got)).To(Succeed())
				var programmed bool
				for _, c := range got.Status.Conditions {
					if c.Type == "Programmed" && c.Status == metav1.ConditionTrue {
						programmed = true
					}
				}
				g.Expect(programmed).To(BeTrue(), "waypoint not Programmed yet")
			}).Should(Succeed())
		}

		By("running one chain probe per scenario (single GET against web)")
		// All scenarios share the same URL; only headers differ. Pack them
		// into one ephemeral pod so we pay cluster-curl warm-up once.
		probesIn := make([]Probe, 0, len(chainScenarios))
		for _, s := range chainScenarios {
			p := Probe{Tag: s.name, URL: "http://" + webHost + "/"}
			if s.cookie != "" {
				p.Headers = []string{"Cookie: " + s.cookie}
			}
			probesIn = append(probesIn, p)
		}
		results := runProbes(ns, probesIn)
		for _, r := range results {
			hops := parseChain(r.Body)
			Expect(hops).To(HaveLen(3),
				"scenario %q: expected 3 hops in chain (web→api→db), got %d.\nbody=%s", r.Tag, len(hops), r.Body)
			probes[r.Tag] = hops
		}
	})

	for _, s := range chainScenarios {
		s := s // capture
		It(s.descShort, Label(s.name), func() {
			hops := probes[s.name]
			cookieDesc := "(no cookie)"
			if s.cookie != "" {
				cookieDesc = "Cookie: " + s.cookie
			}
			req := fmt.Sprintf("GET http://%s/  %s", webHost, cookieDesc)

			expectChainHop(hops, 0, "web", s.wantWeb, req)
			expectChainHop(hops, 1, "api", s.wantAPI, req)
			expectChainHop(hops, 2, "db", s.wantDB, req)
		})
	}
})

// expectChainHop asserts the hop at index i has the expected tier (sanity
// check on chain ordering) and that its pod resolves to the expected
// deployment-level variant. Prints request / expected / observed so a
// failure is legible without re-running.
func expectChainHop(hops []chainHop, i int, wantTier, wantVariant, request string) {
	GinkgoHelper()
	Expect(hops).To(HaveLen(3))
	hop := hops[i]
	got := variantOf(hop.Pod)
	fmt.Fprintf(GinkgoWriter,
		"  request : %s\n  hop %d   : tier=%s expected=%s observed=%s (pod=%s)\n",
		request, i, wantTier, wantVariant, displayVariantHop(got), hop.Pod)
	Expect(hop.Tier).To(Equal(wantTier),
		"chain ordering is broken at hop %d: expected tier=%s, got %s (pod=%s)",
		i, wantTier, hop.Tier, hop.Pod)
	Expect(got).To(Equal(wantVariant),
		"hop %d (tier=%s): expected variant=%s, got %s (pod=%s)",
		i, wantTier, wantVariant, got, hop.Pod)
}

func displayVariantHop(v string) string {
	if v == "" {
		return "<empty>"
	}
	return v
}
