/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package verify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/egoavara/route-prism/internal/preflight"
)

// VerifyNamespace is the dedicated namespace where the probe creates its
// throwaway Service / HTTPRoute. Kept stable so subsequent runs can reuse
// (and so the namespace is easy to spot / delete by hand).
const VerifyNamespace = "route-prism-verify"

// Step is a coarse-grained progress event surfaced to the TUI.
type Step struct {
	Title  string
	Detail string
	OK     *bool // nil = still running; non-nil indicates resolved status
}

// Stream is the channel-driven progress feed produced by Run.
type Stream struct {
	Steps  <-chan Step
	Result <-chan Result
}

// Result is the terminal outcome of a verification run.
type Result struct {
	Report preflight.ProbeReport
	Cases  []TrafficCase
	Err    error
}

// Options bundles tunable parameters for Run.
type Options struct {
	Namespace      string                // defaults to VerifyNamespace
	Timeout        time.Duration         // overall budget; defaults to 180s
	KeepNamespace  bool                  // when true, do not delete the namespace afterwards
	KubeconfigPath string                // optional explicit kubeconfig path
	ContextName    string                // required: which context to run against
	TestImage      string                // backend image with `route-prism test` (defaults to DefaultTestImage)
	MeshOverride   preflight.MeshMode    // when set and mesh is Istio, forces sidecar/ambient regardless of detection
}

// Run kicks off verification asynchronously and returns a Stream the TUI
// can drain. The returned channels are closed when the run finishes.
func Run(ctx context.Context, opts Options) Stream {
	stepsCh := make(chan Step, 16)
	resultCh := make(chan Result, 1)

	if opts.Namespace == "" {
		opts.Namespace = VerifyNamespace
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 180 * time.Second
	}

	go func() {
		defer close(stepsCh)
		defer close(resultCh)

		send := func(title, detail string, ok *bool) {
			select {
			case stepsCh <- Step{Title: title, Detail: detail, OK: ok}:
			case <-ctx.Done():
			}
		}
		ok := func(b bool) *bool { return &b }

		// 1. Build rest.Config + client.
		send("Connecting to cluster", opts.ContextName, nil)
		cfg, err := RestConfigFor(opts.KubeconfigPath, opts.ContextName)
		if err != nil {
			send("Connecting to cluster", err.Error(), ok(false))
			resultCh <- Result{Err: err}
			return
		}
		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(gwv1.Install(scheme))
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			send("Connecting to cluster", err.Error(), ok(false))
			resultCh <- Result{Err: fmt.Errorf("build client: %w", err)}
			return
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			send("Connecting to cluster", err.Error(), ok(false))
			resultCh <- Result{Err: fmt.Errorf("build clientset: %w", err)}
			return
		}
		send("Connecting to cluster", cfg.Host, ok(true))

		// 2. Detect mesh — namespace labels and waypoint setup depend on
		// the mesh kind/mode. Caller may override the mode via
		// Options.MeshOverride (TUI mesh-mode picker / --mesh-mode flag).
		send("Detecting service mesh", "scanning istio-system / kube-system", nil)
		mesh := preflight.DetectMesh(ctx, c)
		if opts.MeshOverride != "" && mesh.Kind == preflight.MeshIstio {
			mesh.Mode = opts.MeshOverride
			send("Detecting service mesh", mesh.Summary()+" (mode override)", ok(true))
		} else {
			send("Detecting service mesh", mesh.Summary(), ok(true))
		}

		// 3. Ensure namespace exists AND carries the mesh-required labels.
		send("Ensuring namespace "+opts.Namespace, "", nil)
		nsAction, err := reconcileNamespace(ctx, c, opts.Namespace, mesh.NamespaceLabels())
		if err != nil {
			send("Ensuring namespace", err.Error(), ok(false))
			resultCh <- Result{Err: err}
			return
		}
		send("Ensuring namespace "+opts.Namespace, nsAction, ok(true))

		// 4. Deep probe — backends + HTTPRoute + traffic verification.
		report, cases := DeepProbe(ctx, c, cs, opts.Namespace, opts.Timeout, opts.TestImage, mesh, send)
		report.Mesh = mesh

		// 4. Cleanup namespace (best-effort; detached ctx so a cancelled
		// parent still lets cleanup finish).
		if !opts.KeepNamespace {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			send("Cleaning up namespace", "", nil)
			if err := deleteNamespace(cleanupCtx, c, opts.Namespace); err != nil {
				send("Cleaning up namespace", err.Error(), ok(false))
			} else {
				send("Cleaning up namespace", "deleted", ok(true))
			}
		} else {
			send("Cleaning up namespace", "skipped (--keep-namespace)", ok(true))
		}

		resultCh <- Result{Report: report, Cases: cases, Err: report.Err}
	}()

	return Stream{Steps: stepsCh, Result: resultCh}
}

// reconcileNamespace creates the namespace (idempotent) AND patches in
// any mesh-required labels that aren't already there. Existing labels are
// preserved — we never strip values the user/operator set deliberately.
//
// Returns a short human-friendly summary of what was done: "created",
// "labels patched: ...", or "already configured".
func reconcileNamespace(ctx context.Context, c client.Client, name string, requiredLabels map[string]string) (string, error) {
	managedLabel := map[string]string{"app.kubernetes.io/managed-by": "route-prism-verify"}
	allRequired := mergeLabels(managedLabel, requiredLabels)

	var existing corev1.Namespace
	getErr := c.Get(ctx, client.ObjectKey{Name: name}, &existing)
	if apierrors.IsNotFound(getErr) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: allRequired}}
		if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create namespace %q: %w", name, err)
		}
		if len(requiredLabels) > 0 {
			return fmt.Sprintf("created with %s", labelsString(requiredLabels)), nil
		}
		return "created", nil
	}
	if getErr != nil {
		return "", fmt.Errorf("inspect namespace %q: %w", name, getErr)
	}

	// Namespace exists. Compute which labels are missing or different.
	missing := map[string]string{}
	for k, v := range allRequired {
		if cur, ok := existing.Labels[k]; !ok || cur != v {
			missing[k] = v
		}
	}
	if len(missing) == 0 {
		return "already configured", nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range missing {
		existing.Labels[k] = v
	}
	if err := c.Patch(ctx, &existing, patch); err != nil {
		return "", fmt.Errorf("patch namespace %q labels: %w", name, err)
	}
	return "labels patched: " + labelsString(missing), nil
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func labelsString(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, ", ")
}

// Inspect connects to the cluster identified by opts.ContextName /
// KubeconfigPath and returns mesh detection information without making
// any modifications. The TUI calls this between context selection and
// the deep probe so the user can confirm or override the detected
// Istio mode (sidecar / ambient).
func Inspect(ctx context.Context, opts Options) (preflight.MeshInfo, error) {
	cfg, err := RestConfigFor(opts.KubeconfigPath, opts.ContextName)
	if err != nil {
		return preflight.MeshInfo{}, err
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return preflight.MeshInfo{}, fmt.Errorf("build client: %w", err)
	}
	return preflight.DetectMesh(ctx, c), nil
}

func deleteNamespace(ctx context.Context, c client.Client, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %q: %w", name, err)
	}
	return nil
}

// Recommend produces a short list of actionable suggestions based on a
// completed report. Used by both the TUI and non-interactive output.
func Recommend(rep preflight.ProbeReport) []string {
	switch rep.Outcome {
	case preflight.OutcomeAccepted:
		var out []string
		if msg := rep.Mesh.KnownIssue(); msg != "" {
			out = append(out, msg)
		}
		if rep.Mesh.Experimental() {
			out = append(out, fmt.Sprintf("%s is in the experimental band — consider upgrading.", rep.Mesh.Summary()))
		}
		if len(out) == 0 {
			out = append(out, "GAMMA is fully operational. route-prism is ready to deploy.")
		}
		return out
	case preflight.OutcomeRejected:
		return []string{
			fmt.Sprintf("Controller %q rejected the probe HTTPRoute (reason=%q).", rep.ControllerName, rep.Reason),
			"Check the controller logs and verify it allows Service parentRefs.",
			"Some meshes require an explicit waypoint or annotation — see the Wiki for compatibility notes.",
		}
	case preflight.OutcomeNoController:
		return []string{
			"No controller picked up the HTTPRoute. Likely causes:",
			"  • No GAMMA-aware mesh installed (Istio ≥ 1.22, Cilium ≥ 1.16, Linkerd …).",
			"  • Mesh installed but missing the GAMMA / mesh-routing feature flag.",
			"  • RBAC prevents the controller from watching this namespace.",
		}
	case preflight.OutcomeCRDMissing:
		return []string{
			"HTTPRoute CRD is missing. Install Gateway API CRDs first:",
			"  kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml",
			"Then install a GAMMA-aware mesh (Istio 1.22+, Cilium 1.16+, …).",
		}
	case preflight.OutcomeProbeError:
		return []string{
			"Probe setup failed before any controller could respond:",
			"  " + errString(rep.Err),
			"Verify that you have create/delete permissions on Service and HTTPRoute in the target namespace.",
		}
	}
	return nil
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	if errors.Is(e, preflight.ErrCRDMissing) {
		return "HTTPRoute CRD not installed"
	}
	return e.Error()
}
