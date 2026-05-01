/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package verify

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/egoavara/route-prism/internal/preflight"
)

// Names for the deep-probe scaffolding. Stable so repeated runs reuse the
// objects (and are cleaned up together with the namespace at teardown).
const (
	deepTargetSvc   = "verify-target"
	deepVariantA    = "verify-variant-a"
	deepVariantB    = "verify-variant-b"
	deepRouteName   = "verify-route"
	deepClientPod   = "verify-client"
	deepCurlImage   = "curlimages/curl:8.10.1"
	deepEchoPort    = 8080
	deepRoutingKey  = "x-route-prism"
	deepCurlTimeout = 5 // seconds per curl invocation
)

// DefaultTestImage is the operator image used as the deep-probe backend.
// It must contain the `test` subcommand (a tiny self-identifying HTTP
// server). Override per-call via Options.TestImage when validating a
// pre-release build.
const DefaultTestImage = "ghcr.io/egoavara/route-prism:latest"

// TrafficCase is the outcome of one curl probe through the HTTPRoute.
type TrafficCase struct {
	Name        string // human-readable description ("variant-a baggage")
	Header      string // raw `baggage` header sent (empty = no header)
	Expected    string // expected hostname / variant identifier in the response
	GotVariant  string // parsed variant identifier from the echo response
	OK          bool
	RawResponse string
	Err         error
}

// DeepProbe performs an end-to-end GAMMA verification:
//
//  1. Deploys two echo-server backends (variant-a, variant-b) and one
//     parent Service that an HTTPRoute attaches to.
//  2. Renders an HTTPRoute matching `baggage: x-route-prism=<variant>`
//     and waits until a controller marks Accepted=True.
//  3. Spawns an in-cluster curl Pod that fires three requests:
//     variant-a, variant-b, and a no-header default — then verifies
//     that each lands on the expected backend.
//
// All resources live in `namespace` (which the caller created beforehand)
// and are deleted via namespace cascade by the orchestrator.
//
// `send` is invoked for each step transition so the TUI can stream
// progress; pass a no-op closure for non-streaming use.
func DeepProbe(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	namespace string,
	timeout time.Duration,
	testImage string,
	mesh preflight.MeshInfo,
	send func(title, detail string, ok *bool),
) (preflight.ProbeReport, []TrafficCase) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if testImage == "" {
		testImage = DefaultTestImage
	}
	ok := func(b bool) *bool { return &b }
	deadline := time.Now().Add(timeout)

	report := preflight.ProbeReport{Mesh: mesh}

	// Variant Service annotations vary per mesh (Istio ambient needs
	// `istio.io/use-waypoint`; sidecar needs nothing extra).
	svcAnnotations := serviceAnnotations(mesh)

	// 1. Deploy backends.
	send("Deploying test backends", testImage, nil)
	if err := deployEcho(ctx, c, namespace, deepVariantA, testImage, svcAnnotations); err != nil {
		send("Deploying test backends", err.Error(), ok(false))
		report.Outcome = preflight.OutcomeProbeError
		report.Err = err
		return report, nil
	}
	if err := deployEcho(ctx, c, namespace, deepVariantB, testImage, svcAnnotations); err != nil {
		send("Deploying test backends", err.Error(), ok(false))
		report.Outcome = preflight.OutcomeProbeError
		report.Err = err
		return report, nil
	}
	if err := deployTargetService(ctx, c, namespace, svcAnnotations); err != nil {
		send("Deploying test backends", err.Error(), ok(false))
		report.Outcome = preflight.OutcomeProbeError
		report.Err = err
		return report, nil
	}

	// 1.5 Deploy a waypoint Gateway for Istio ambient — without it L7
	// HTTPRoute matching is silently skipped.
	if mesh.RequiresWaypoint() {
		send("Deploying waypoint Gateway", "istio-waypoint", nil)
		if err := deployWaypoint(ctx, c, namespace); err != nil {
			send("Deploying waypoint Gateway", err.Error(), ok(false))
			report.Outcome = preflight.OutcomeProbeError
			report.Err = err
			return report, nil
		}
		send("Deploying waypoint Gateway", "ready", ok(true))
	}

	if err := waitForDeploymentReady(ctx, c, namespace, deepVariantA, until(deadline)); err != nil {
		send("Deploying test backends", "variant-a not ready: "+err.Error(), ok(false))
		report.Outcome = preflight.OutcomeProbeError
		report.Err = err
		return report, nil
	}
	if err := waitForDeploymentReady(ctx, c, namespace, deepVariantB, until(deadline)); err != nil {
		send("Deploying test backends", "variant-b not ready: "+err.Error(), ok(false))
		report.Outcome = preflight.OutcomeProbeError
		report.Err = err
		return report, nil
	}
	send("Deploying test backends", "ready", ok(true))

	// 2. Apply HTTPRoute and wait for Accepted.
	send("Applying HTTPRoute", deepRouteName, nil)
	hr := buildHTTPRoute(namespace)
	if err := c.Create(ctx, hr); err != nil {
		if meta.IsNoMatchError(err) {
			send("Applying HTTPRoute", "HTTPRoute CRD missing", ok(false))
			report.Outcome = preflight.OutcomeCRDMissing
			return report, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			send("Applying HTTPRoute", err.Error(), ok(false))
			report.Outcome = preflight.OutcomeProbeError
			report.Err = err
			return report, nil
		}
	}
	if err := waitForHTTPRouteAccepted(ctx, c, hr, until(deadline)); err != nil {
		report.Outcome = classifyAcceptanceErr(err)
		report.Err = err
		send("Applying HTTPRoute", err.Error(), ok(false))
		return report, nil
	}
	send("Applying HTTPRoute", "Accepted", ok(true))
	report.Outcome = preflight.OutcomeAccepted

	// 3. Run traffic probes.
	send("Running traffic probes", "curl × 3", nil)
	cases, err := runTrafficProbes(ctx, c, cs, namespace, until(deadline))
	if err != nil {
		send("Running traffic probes", err.Error(), ok(false))
		return report, cases
	}
	allPass := true
	for _, tc := range cases {
		if !tc.OK {
			allPass = false
			break
		}
	}
	if allPass {
		send("Running traffic probes", "all 3 cases passed", ok(true))
	} else {
		send("Running traffic probes", "one or more cases failed", ok(false))
	}
	return report, cases
}

// ---------- builders ----------

func until(deadline time.Time) time.Duration {
	d := time.Until(deadline)
	if d < 0 {
		return 0
	}
	return d
}

func echoLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/managed-by": "route-prism-verify",
	}
}

// serviceAnnotations returns Service-level annotations / labels needed
// for a given mesh (e.g. `istio.io/use-waypoint` for ambient).
func serviceAnnotations(m preflight.MeshInfo) map[string]string {
	if m.RequiresWaypoint() {
		return map[string]string{"istio.io/use-waypoint": deepWaypointName}
	}
	return nil
}

const deepWaypointName = "verify-waypoint"

// deployWaypoint creates the Istio ambient waypoint Gateway in `ns`.
// It uses the Istio-managed gatewayClass `istio-waypoint`; istiod
// instantiates the actual waypoint Pod from this resource. The
// `istio.io/waypoint-for: service` label scopes the waypoint to handle
// L7 traffic destined for Services that reference it.
func deployWaypoint(ctx context.Context, c client.Client, ns string) error {
	gw := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deepWaypointName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "route-prism-verify",
				"istio.io/waypoint-for":        "service",
			},
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
	if err := c.Create(ctx, gw); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create waypoint Gateway: %w", err)
	}
	return nil
}

func deployEcho(ctx context.Context, c client.Client, ns, name, image string, svcLabelExtras map[string]string) error {
	labels := echoLabels(name)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "test",
						Image:   image,
						Command: []string{"/manager"},
						Args:    []string{"test", "--addr", fmt.Sprintf(":%d", deepEchoPort)},
						Env: []corev1.EnvVar{
							{Name: "VARIANT_NAME", Value: name},
						},
						Ports: []corev1.ContainerPort{{ContainerPort: deepEchoPort}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt(deepEchoPort),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       2,
						},
					}},
				},
			},
		},
	}
	if err := c.Create(ctx, dep); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment %q: %w", name, err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: mergeLabels(labels, svcLabelExtras)},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:        "http",
				Port:        80,
				TargetPort:  intstr.FromInt(deepEchoPort),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: ptr.To("http"),
			}},
		},
	}
	if err := c.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service %q: %w", name, err)
	}
	return nil
}

// deployTargetService creates the GAMMA "frontend" Service that the
// HTTPRoute attaches to via parentRef. It has no selector — traffic is
// redirected by the mesh/HTTPRoute layer, never by Service endpoints.
func deployTargetService(ctx context.Context, c client.Client, ns string, labelExtras map[string]string) error {
	labels := mergeLabels(map[string]string{
		"app.kubernetes.io/managed-by": "route-prism-verify",
		"app.kubernetes.io/component":  "target",
	}, labelExtras)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deepTargetSvc,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:        "http",
				Port:        80,
				TargetPort:  intstr.FromInt(deepEchoPort),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: ptr.To("http"),
			}},
			// Intentionally no selector — pure routing target.
			Selector: map[string]string{
				"route-prism.egoavara.net/verify-target": "true",
			},
		},
	}
	if err := c.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create target service: %w", err)
	}
	return nil
}

func buildHTTPRoute(ns string) *gwv1.HTTPRoute {
	regexType := gwv1.HeaderMatchRegularExpression
	port := gwv1.PortNumber(80)
	coreGroup := gwv1.Group("")
	svcKind := gwv1.Kind("Service")

	headerRule := func(variant string) gwv1.HTTPRouteRule {
		return gwv1.HTTPRouteRule{
			Matches: []gwv1.HTTPRouteMatch{{
				Headers: []gwv1.HTTPHeaderMatch{{
					Type:  &regexType,
					Name:  "baggage",
					Value: baggageRegex(deepRoutingKey, variant),
				}},
			}},
			BackendRefs: []gwv1.HTTPBackendRef{{
				BackendRef: gwv1.BackendRef{
					BackendObjectReference: gwv1.BackendObjectReference{
						Group: &coreGroup,
						Name:  gwv1.ObjectName(variant),
						Port:  &port,
					},
				},
			}},
		}
	}
	catchAll := gwv1.HTTPRouteRule{
		BackendRefs: []gwv1.HTTPBackendRef{{
			BackendRef: gwv1.BackendRef{
				BackendObjectReference: gwv1.BackendObjectReference{
					Group: &coreGroup,
					Name:  gwv1.ObjectName(deepVariantA),
					Port:  &port,
				},
			},
		}},
	}

	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deepRouteName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "route-prism-verify",
			},
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Group: &coreGroup,
					Kind:  &svcKind,
					Name:  gwv1.ObjectName(deepTargetSvc),
				}},
			},
			Rules: []gwv1.HTTPRouteRule{
				headerRule(deepVariantA),
				headerRule(deepVariantB),
				catchAll,
			},
		},
	}
}

// baggageRegex anchors a single member match against the full baggage
// header value; mirrors internal/controller/render_httproute.go.
func baggageRegex(routingKey, value string) string {
	return `(?:.*,)?\s*` + regexpQuote(routingKey) + `=` + regexpQuote(value) + `(?:;[^,]*)?\s*(?:,.*)?`
}

// regexpQuote duplicates the small subset of regexp.QuoteMeta that we
// need; we keep the dependency tree light here.
func regexpQuote(s string) string {
	const special = `\.+*?()|[]{}^$`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- waiters ----------

func waitForDeploymentReady(ctx context.Context, c client.Client, ns, name string, budget time.Duration) error {
	if budget <= 0 {
		budget = 60 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return wait.PollUntilContextCancel(wctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var d appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
			return false, nil
		}
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		return d.Status.ReadyReplicas >= desired, nil
	})
}

func waitForHTTPRouteAccepted(ctx context.Context, c client.Client, hr *gwv1.HTTPRoute, budget time.Duration) error {
	if budget <= 0 {
		budget = 30 * time.Second
	}
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var lastReason, lastMsg string
	timedOut := true
	err := wait.PollUntilContextCancel(wctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var current gwv1.HTTPRoute
		if err := c.Get(ctx, client.ObjectKeyFromObject(hr), &current); err != nil {
			return false, nil
		}
		if len(current.Status.Parents) == 0 {
			return false, nil
		}
		for _, p := range current.Status.Parents {
			for _, cond := range p.Conditions {
				if cond.Type != string(gwv1.RouteConditionAccepted) {
					continue
				}
				timedOut = false
				if cond.Status == metav1.ConditionTrue {
					return true, nil
				}
				lastReason, lastMsg = cond.Reason, cond.Message
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil && timedOut {
		return errNoController
	}
	if lastReason != "" {
		return fmt.Errorf("HTTPRoute rejected: reason=%s msg=%s", lastReason, lastMsg)
	}
	return nil
}

var errNoController = errors.New("no controller wrote HTTPRoute status before timeout")

func classifyAcceptanceErr(err error) preflight.ProbeOutcome {
	if errors.Is(err, errNoController) {
		return preflight.OutcomeNoController
	}
	return preflight.OutcomeRejected
}

// ---------- traffic ----------

func runTrafficProbes(ctx context.Context, c client.Client, cs kubernetes.Interface, ns string, budget time.Duration) ([]TrafficCase, error) {
	if budget <= 0 {
		budget = 60 * time.Second
	}

	cases := []TrafficCase{
		{Name: "baggage routes to variant-a", Header: deepRoutingKey + "=" + deepVariantA, Expected: deepVariantA},
		{Name: "baggage routes to variant-b", Header: deepRoutingKey + "=" + deepVariantB, Expected: deepVariantB},
		{Name: "no header → catch-all (variant-a)", Header: "", Expected: deepVariantA},
	}

	pod := buildCurlPod(ns, cases)
	// Best-effort cleanup of any leftover from a previous run.
	_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: deepClientPod, Namespace: ns}})

	if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		err := c.Create(ctx, pod)
		if err == nil || apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return cases, fmt.Errorf("create curl pod: %w", err)
	}

	// Wait for completion.
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var finalPhase corev1.PodPhase
	err := wait.PollUntilContextCancel(wctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &p); err != nil {
			return false, nil
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			finalPhase = p.Status.Phase
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return cases, fmt.Errorf("curl pod did not complete: %w", err)
	}

	logs, logErr := podLogs(ctx, cs, ns, deepClientPod)
	if logErr != nil {
		return cases, fmt.Errorf("read curl pod logs: %w", logErr)
	}
	parseTrafficLogs(logs, cases)

	if finalPhase == corev1.PodFailed {
		// Logs already parsed; surface failure flag implicitly via Cases.
	}
	return cases, nil
}

func buildCurlPod(ns string, cases []TrafficCase) *corev1.Pod {
	// One curl per case; each prints "CASE-<i>:<status>:<body>".
	var lines []string
	for i, tc := range cases {
		header := ""
		if tc.Header != "" {
			header = fmt.Sprintf("-H 'baggage: %s'", tc.Header)
		}
		lines = append(lines, fmt.Sprintf(
			`code=$(curl -sS -m %d -o /tmp/body -w '%%{http_code}' %s http://%s/ || echo 000); body=$(cat /tmp/body 2>/dev/null | tr '\n' ' ' | head -c 1024); printf 'CASE-%d:%%s:%%s\n' "$code" "$body"`,
			deepCurlTimeout, header, deepTargetSvc, i,
		))
	}
	script := "set -u\n" + strings.Join(lines, "\n") + "\n"

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deepClientPod,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "route-prism-verify",
				"app.kubernetes.io/component":  "client",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "curl",
				Image:   deepCurlImage,
				Command: []string{"sh", "-c", script},
			}},
		},
	}
}

// parseTrafficLogs walks the curl-pod stdout and fills in each case's
// GotVariant / OK / RawResponse fields. The echo image
// ghcr.io/mendhak/http-https-echo embeds the served pod's hostname under
// "host" / "hostname" — we look for the variant name as a substring of
// the response body, which is enough for "is the right backend?".
func parseTrafficLogs(logs string, cases []TrafficCase) {
	scan := bufio.NewScanner(strings.NewReader(logs))
	scan.Buffer(make([]byte, 1<<20), 1<<20)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "CASE-") {
			continue
		}
		// CASE-<i>:<code>:<body>
		rest := strings.TrimPrefix(line, "CASE-")
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) < 3 {
			continue
		}
		var idx int
		_, _ = fmt.Sscanf(parts[0], "%d", &idx)
		if idx < 0 || idx >= len(cases) {
			continue
		}
		code, body := parts[1], parts[2]
		cases[idx].RawResponse = fmt.Sprintf("HTTP %s | %s", code, body)
		if code != "200" {
			cases[idx].OK = false
			cases[idx].Err = fmt.Errorf("HTTP %s", code)
			continue
		}
		// http-https-echo prints JSON with "hostname"/"host" containing
		// the pod hostname (== Deployment name + suffix). Substring match.
		got := ""
		switch {
		case strings.Contains(body, deepVariantA):
			got = deepVariantA
		case strings.Contains(body, deepVariantB):
			got = deepVariantB
		}
		cases[idx].GotVariant = got
		cases[idx].OK = (got == cases[idx].Expected)
		if !cases[idx].OK && cases[idx].Err == nil {
			cases[idx].Err = fmt.Errorf("expected %q, got %q", cases[idx].Expected, got)
		}
	}
}

func podLogs(ctx context.Context, cs kubernetes.Interface, ns, name string) (string, error) {
	req := cs.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{})
	rc, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
