//go:build routing

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// ── platform detection ──────────────────────────────────────────────

func detectPlatform(ctx context.Context) Platform {
	var gcs gwv1.GatewayClassList
	Expect(k8sClient.List(ctx, &gcs)).To(Succeed(), "could not list GatewayClass")

	hasCilium, hasIstio := false, false
	for _, gc := range gcs.Items {
		switch gc.Name {
		case "cilium":
			hasCilium = true
		case "istio":
			hasIstio = true
		}
	}
	switch {
	case hasCilium && hasIstio:
		return PlatformCiliumIstio
	case hasIstio:
		return PlatformIstio
	case hasCilium:
		return PlatformCilium
	}
	Fail("no 'cilium' or 'istio' GatewayClass — run ./hack/kind-up.sh first")
	return ""
}

// ── namespace + waypoint setup ──────────────────────────────────────

// setupNamespace creates (or reuses) a namespace labelled for this suite,
// enrolled into Istio ambient. On Istio-class platforms it also provisions a
// namespace-scoped waypoint so HTTPRoutes attached to Services in the
// namespace are actually enforced at L7.
func setupNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				NSLabelKey:                NSLabelValue,
				"istio.io/dataplane-mode": "ambient",
			},
		},
	}
	createOrPatch(ctx, ns, func(obj client.Object) {
		got := obj.(*corev1.Namespace)
		if got.Labels == nil {
			got.Labels = map[string]string{}
		}
		got.Labels[NSLabelKey] = NSLabelValue
		got.Labels["istio.io/dataplane-mode"] = "ambient"
	})

	if !platform.IsIstioDataplane() {
		return
	}

	// Waypoint Gateway.
	gw := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "waypoint",
			Namespace: name,
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
	createOrPatch(ctx, gw, func(_ client.Object) {})

	// Tag the namespace to use this waypoint.
	var fresh corev1.Namespace
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &fresh)).To(Succeed())
	if fresh.Labels["istio.io/use-waypoint"] != "waypoint" {
		if fresh.Labels == nil {
			fresh.Labels = map[string]string{}
		}
		fresh.Labels["istio.io/use-waypoint"] = "waypoint"
		Expect(k8sClient.Update(ctx, &fresh)).To(Succeed())
	}

	By(fmt.Sprintf("waiting for waypoint Gateway %s/waypoint to be Programmed", name))
	Eventually(func(g Gomega) {
		var got gwv1.Gateway
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: "waypoint"}, &got)).To(Succeed())
		var programmed bool
		for _, c := range got.Status.Conditions {
			if c.Type == "Programmed" && c.Status == metav1.ConditionTrue {
				programmed = true
			}
		}
		g.Expect(programmed).To(BeTrue(), "waypoint not Programmed yet")
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// listSuiteNamespaces returns the namespaces this suite owns (excluding the
// devloop `demo` namespace, which is owned by Tilt).
func listSuiteNamespaces(ctx context.Context) []corev1.Namespace {
	var nss corev1.NamespaceList
	if err := k8sClient.List(ctx, &nss, client.MatchingLabels{NSLabelKey: NSLabelValue}); err != nil {
		return nil
	}
	out := make([]corev1.Namespace, 0, len(nss.Items))
	for _, n := range nss.Items {
		if n.Name == "demo" {
			continue
		}
		out = append(out, n)
	}
	return out
}

// createOrPatch creates obj if absent; otherwise applies mutate to the live
// object and Updates. Idempotent across re-runs.
func createOrPatch(ctx context.Context, obj client.Object, mutate func(client.Object)) {
	if err := k8sClient.Create(ctx, obj); err == nil || apierrors.IsAlreadyExists(err) {
		if err == nil {
			return
		}
	} else {
		Expect(err).NotTo(HaveOccurred(), "create failed")
		return
	}
	// AlreadyExists path: get + mutate + update.
	live := obj.DeepCopyObject().(client.Object)
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
	mutate(live)
	Expect(k8sClient.Update(ctx, live)).To(Succeed())
}

// ── workload deployment ─────────────────────────────────────────────

// AppProtocol toggles whether the Service port carries appProtocol=http.
type AppProtocol int

const (
	AppProtoHTTP AppProtocol = iota // default — emit appProtocol: http
	AppProtoNone                    // emit no appProtocol field (G8 negative case)
)

func deployWorkload(ctx context.Context, ns, name, variant string, appProto AppProtocol) {
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web", "variant": variant}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web", "variant": variant}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "echo",
						Image: "ealen/echo-server:0.9.2",
						Env:   []corev1.EnvVar{{Name: "PORT", Value: "80"}},
						Ports: []corev1.ContainerPort{{ContainerPort: 80}},
					}},
				},
			},
		},
	}
	createOrPatch(ctx, dep, func(_ client.Object) {})

	port := corev1.ServicePort{
		Port:       80,
		TargetPort: intstr.FromInt(80),
		Name:       "http",
	}
	if appProto == AppProtoHTTP {
		ap := "http"
		port.AppProtocol = &ap
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web", "variant": variant},
			Ports:    []corev1.ServicePort{port},
		},
	}
	createOrPatch(ctx, svc, func(o client.Object) {
		// keep selector/ports stable across re-runs (cluster IP is preserved).
		got := o.(*corev1.Service)
		got.Spec.Selector = svc.Spec.Selector
		got.Spec.Ports = svc.Spec.Ports
	})

	By(fmt.Sprintf("waiting for Deployment %s/%s to be Ready", ns, name))
	Eventually(func(g Gomega) {
		var d appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
		g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1))
	}).Should(Succeed())
}

// ── CR / ET helpers ─────────────────────────────────────────────────

func applyContextRoute(ctx context.Context, ns, name, target string, variantSelector map[string]string) {
	cr := &routeprismv1alpha1.ContextRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: routeprismv1alpha1.ContextRouteSpec{
			Target: routeprismv1alpha1.TargetReference{
				Service: routeprismv1alpha1.ServiceReference{Name: target},
			},
			Variants: routeprismv1alpha1.VariantSelector{
				Selector: metav1.LabelSelector{MatchLabels: variantSelector},
			},
		},
	}
	createOrPatch(ctx, cr, func(o client.Object) {
		got := o.(*routeprismv1alpha1.ContextRoute)
		got.Spec = cr.Spec
	})
}

func applyEdgeTransformation(ctx context.Context, ns, name, target, mode, sourceCookie string) {
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: routeprismv1alpha1.EdgeTransformationSpec{
			Mode:         routeprismv1alpha1.EdgeMode(mode),
			SourceCookie: sourceCookie,
			Target: routeprismv1alpha1.TargetReference{
				Service: routeprismv1alpha1.ServiceReference{Name: target},
			},
		},
	}
	createOrPatch(ctx, et, func(o client.Object) {
		got := o.(*routeprismv1alpha1.EdgeTransformation)
		got.Spec = et.Spec
	})
}

// ── status / event assertions ───────────────────────────────────────

// expectReady polls the named CR/ET until its Ready condition matches the
// expected status+reason, or fails the spec.
func expectReady(ctx context.Context, ns string, obj client.Object, wantStatus metav1.ConditionStatus, wantReason string) {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		// fall back to type name
		kind = fmt.Sprintf("%T", obj)
	}
	name := obj.GetName()
	By(fmt.Sprintf("expecting %s/%s in %s to reach Ready=%s/%s", kind, name, ns, wantStatus, wantReason))
	Eventually(func(g Gomega) {
		fresh := obj.DeepCopyObject().(client.Object)
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, fresh)).To(Succeed())
		conds := readyConditions(fresh)
		var ready *metav1.Condition
		for i := range conds {
			if conds[i].Type == "Ready" {
				ready = &conds[i]
				break
			}
		}
		g.Expect(ready).NotTo(BeNil(), "no Ready condition yet")
		g.Expect(ready.Status).To(Equal(wantStatus),
			"wanted Ready=%s, got %s", wantStatus, ready.Status)
		g.Expect(ready.Reason).To(Equal(wantReason),
			"wanted Reason=%s, got %s (msg=%q)", wantReason, ready.Reason, ready.Message)
	}).Should(Succeed())
}

func readyConditions(obj client.Object) []metav1.Condition {
	switch v := obj.(type) {
	case *routeprismv1alpha1.ContextRoute:
		return v.Status.Conditions
	case *routeprismv1alpha1.EdgeTransformation:
		return v.Status.Conditions
	}
	return nil
}

// expectWarningEvent fails the spec unless at least one Warning Event with
// the given reason is observed for the involvedObject within Eventually's
// budget.
func expectWarningEvent(ctx context.Context, ns, kind, name, reason string) {
	By(fmt.Sprintf("expecting Warning Event reason=%s on %s/%s in %s", reason, kind, name, ns))
	Eventually(func(g Gomega) {
		var evs corev1.EventList
		g.Expect(k8sClient.List(ctx, &evs, client.InNamespace(ns))).To(Succeed())
		var matched int
		for _, e := range evs.Items {
			if e.Reason == reason &&
				e.Type == corev1.EventTypeWarning &&
				e.InvolvedObject.Kind == kind &&
				e.InvolvedObject.Name == name {
				matched++
			}
		}
		g.Expect(matched).To(BeNumerically(">=", 1),
			"no Warning Event with reason=%s on %s/%s yet", reason, kind, name)
	}, 90*time.Second, 2*time.Second).Should(Succeed())
}

// ── in-cluster curl probes ──────────────────────────────────────────

// Probe is one HTTP request to issue inside an ephemeral curl pod.
type Probe struct {
	Tag     string   // sentinel used to demux the multi-probe pod's output
	URL     string   // full URL including http://host[:port]/path
	Headers []string // each header as "Name: value"
}

// ProbeResult is the receiving side: the raw JSON body and the variant label
// derived from .environment.HOSTNAME.
type ProbeResult struct {
	Tag     string
	Body    string
	Pod     string
	Variant string
}

// runProbes spins up one ephemeral curl pod that fires every Probe, demuxes
// its output, and returns one ProbeResult per Probe. The bash-side
// implementation lived in run_probes(); this is the typed equivalent.
//
// Returns one entry per input probe in the same order. Each result's
// Variant is one of: "stable" | "canary" | "v1" | "v2" | "" (empty body)
// | "unknown:<hostname>".
func runProbes(ns string, probes []Probe) []ProbeResult {
	GinkgoHelper()
	Expect(probes).NotTo(BeEmpty())

	// Build the shell program. Same shape as the legacy bash version: warm
	// up unique URLs first, then 8x retry per real probe so cold ztunnel
	// mTLS doesn't poison the first response.
	var script strings.Builder
	script.WriteString("set -u\n")
	script.WriteString("hit() {\n")
	script.WriteString("  for try in 1 2 3 4 5 6 7 8; do\n")
	script.WriteString("    body=$(curl -sS --max-time 10 \"$@\" 2>/dev/null || true)\n")
	script.WriteString("    if [ -n \"$body\" ]; then printf '%s' \"$body\"; return 0; fi\n")
	script.WriteString("    sleep 1\n")
	script.WriteString("  done\n")
	script.WriteString("  echo __CURL_FAILED__\n")
	script.WriteString("}\n")

	seen := map[string]bool{}
	for _, p := range probes {
		if seen[p.URL] {
			continue
		}
		seen[p.URL] = true
		fmt.Fprintf(&script, "curl -sS --max-time 5 %q >/dev/null 2>&1 || true\n", p.URL)
	}
	script.WriteString("sleep 1\n")

	for _, p := range probes {
		fmt.Fprintf(&script, "echo '===%s==='\n", p.Tag)
		script.WriteString("hit")
		for _, h := range p.Headers {
			fmt.Fprintf(&script, " -H %s", shellQuote(h))
		}
		fmt.Fprintf(&script, " %s\n", shellQuote(p.URL))
		script.WriteString("echo\n")
	}
	script.WriteString("echo '===END==='\n")

	pod := fmt.Sprintf("route-prism-routing-%s-%d", ns, time.Now().UnixNano()%100000)
	args := []string{
		"-n", ns, "run", pod, "--rm", "-i", "--restart=Never", "-q",
		"--image=curlimages/curl:8.10.1",
		"--command", "--", "sh", "-c", script.String(),
	}
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("kubectl run failed: %v\n--- output ---\n%s", err, string(out)))
	}
	raw := string(out)

	results := make([]ProbeResult, 0, len(probes))
	for _, p := range probes {
		body := blockBetween(raw, "==="+p.Tag+"===")
		host := extractHost(body)
		results = append(results, ProbeResult{
			Tag:     p.Tag,
			Body:    body,
			Pod:     host,
			Variant: variantOf(host),
		})
	}
	return results
}

// shellQuote produces a single-quoted POSIX shell literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// blockBetween returns the lines of `raw` between `start` and the next line
// beginning with `===`. Mirrors the awk one-liner used in bash.
func blockBetween(raw, start string) string {
	lines := strings.Split(raw, "\n")
	var (
		out  []string
		open bool
	)
	for _, ln := range lines {
		if !open {
			if ln == start {
				open = true
			}
			continue
		}
		if strings.HasPrefix(ln, "===") {
			break
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// extractHost returns the receiving pod's hostname from the response body.
// Two workload images are in use across the suite:
//
//   - sample-tier (devloop chain):   top-level field "pod":"<podname>"
//   - ealen/echo-server (regression): nested .environment.HOSTNAME
//
// We try the sample-tier shape first; for the chained response only the
// outermost hop's pod is returned here. The full chain is walked by
// parseChain (see below).
func extractHost(body string) string {
	if v := scanField(body, `"pod":"`); v != "" {
		return v
	}
	return scanField(body, `"HOSTNAME":"`)
}

func scanField(body, key string) string {
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// chainHop is one node in the response tree returned by sample-tier when
// it chains web → api → db.
type chainHop struct {
	Tier string `json:"tier"`
	Pod  string `json:"pod"`
}

// parseChain walks the JSON body produced by sample-tier and returns one
// entry per visited hop in order (outermost first). Returns nil if the
// body isn't sample-tier shaped.
func parseChain(body string) []chainHop {
	var top map[string]any
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return nil
	}
	var hops []chainHop
	cur := top
	for cur != nil {
		tier, _ := cur["tier"].(string)
		pod, _ := cur["pod"].(string)
		if tier == "" && pod == "" {
			break
		}
		hops = append(hops, chainHop{Tier: tier, Pod: pod})
		next, ok := cur["downstream"].(map[string]any)
		if !ok {
			break
		}
		cur = next
	}
	return hops
}

// variantOf maps a pod hostname back to its Deployment name. Kubernetes
// pod names are `<deployment>-<rs-hash>-<random5>`, so we strip the trailing
// two dash-segments. For the sample / regression workloads this yields the
// deployment label directly:
//   web-7f9b-xxxxx        → "web"
//   web-canary-7f9b-xxxxx → "web-canary"
//   api-7f9b-xxxxx        → "api"
//   api-canary-7f9b-xxxxx → "api-canary"
//   web-v1-7f9b-xxxxx     → "web-v1"
//
// Tests assert on the deployment-level label, which makes per-tier and
// per-variant checks unambiguous.
func variantOf(host string) string {
	if host == "" {
		return ""
	}
	parts := strings.Split(host, "-")
	if len(parts) < 3 {
		return "unknown:" + host
	}
	return strings.Join(parts[:len(parts)-2], "-")
}

// expectVariant prints request / expected / observed in the spec output so
// failures are immediately legible without re-running.
func expectVariant(r ProbeResult, expected, request string) {
	GinkgoHelper()
	fmt.Fprintf(GinkgoWriter,
		"  request : %s\n  expected: variant=%s\n  observed: variant=%s (pod=%s)\n",
		request, expected, displayVariant(r), r.Pod)
	Expect(r.Variant).To(Equal(expected),
		"request: %s\n  expected variant=%s, got variant=%s (pod=%s)\n  body=%s",
		request, expected, displayVariant(r), r.Pod, r.Body)
}

func displayVariant(r ProbeResult) string {
	if r.Variant == "" {
		return "<empty body>"
	}
	return r.Variant
}
