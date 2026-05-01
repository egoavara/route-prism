/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package verify

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// OnExistingPolicy controls what to do when the verify namespace already
// exists and the mesh requires labels that aren't present yet.
type OnExistingPolicy string

const (
	// OnExistingAbort (default) refuses to proceed without explicit user
	// consent. Patching mesh labels onto a pre-existing namespace can
	// silently change the dataplane behaviour of unrelated workloads.
	OnExistingAbort OnExistingPolicy = ""
	// OnExistingDelete deletes the namespace (and everything in it) and
	// recreates it fresh. Safe when the namespace name is the dedicated
	// verify default; explicitly destructive when the user typed in
	// another namespace.
	OnExistingDelete OnExistingPolicy = "delete"
	// OnExistingPatch keeps the namespace and patches the missing labels
	// in. The user has acknowledged that the namespace contains only
	// resources they're willing to expose to mesh-mode changes.
	OnExistingPatch OnExistingPolicy = "patch"
)

// Options bundles tunable parameters for Run.
type Options struct {
	Namespace      string             // defaults to VerifyNamespace
	Timeout        time.Duration      // overall budget; defaults to 180s
	KeepNamespace  bool               // when true, do not delete the namespace afterwards
	KubeconfigPath string             // optional explicit kubeconfig path
	ContextName    string             // required: which context to run against
	TestImage      string             // backend image with `route-prism test` (defaults to DefaultTestImage)
	MeshOverride   preflight.MeshMode // when set and mesh is Istio, forces sidecar/ambient regardless of detection
	OnExisting     OnExistingPolicy   // policy when the namespace exists with missing mesh labels
}

// NamespaceState is what Inspect reports about the verify namespace.
type NamespaceState struct {
	Exists         bool
	ExistingLabels map[string]string
	MissingLabels  map[string]string // labels the mesh requires that aren't present yet
}

// NeedsConsent is true when proceeding would modify a pre-existing
// namespace's labels — the caller must surface a confirmation prompt
// (TUI) or require an explicit policy flag (CLI).
func (n NamespaceState) NeedsConsent() bool {
	return n.Exists && len(n.MissingLabels) > 0
}

// Inspection bundles non-destructive cluster checks the TUI runs between
// context selection and the deep probe.
type Inspection struct {
	Mesh      preflight.MeshInfo
	Namespace NamespaceState
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
		// Pre-existing namespaces are protected: we never silently patch
		// mesh labels (which would change dataplane behaviour for any
		// other workload sharing the namespace). Caller must opt in via
		// OnExisting=delete | patch when missing labels are detected.
		send("Ensuring namespace "+opts.Namespace, "", nil)
		nsAction, err := reconcileNamespace(ctx, c, opts.Namespace, mesh.NamespaceLabels(), opts.OnExisting)
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

// reconcileNamespace ensures the namespace exists and carries the
// required mesh labels, but it NEVER silently patches a pre-existing
// namespace. Behaviour matrix:
//
//   - Namespace doesn't exist → create with all labels.
//   - Namespace exists, all required labels present → no-op.
//   - Namespace exists, missing labels:
//     policy=abort (default) → return ErrNamespaceConsentRequired.
//     policy=delete         → cascade delete the namespace, then
//     recreate with the required labels.
//     policy=patch          → patch only the missing labels in place
//     (existing labels untouched).
//
// Returns a short human-friendly summary suitable for a TUI step.
func reconcileNamespace(
	ctx context.Context,
	c client.Client,
	name string,
	meshLabels map[string]string,
	policy OnExistingPolicy,
) (string, error) {
	// Bookkeeping label — safe to add unilaterally because it does not
	// alter dataplane behaviour. Only the mesh labels gate consent.
	managedLabel := map[string]string{"app.kubernetes.io/managed-by": "route-prism-verify"}
	allOnCreate := mergeLabels(managedLabel, meshLabels)

	var existing corev1.Namespace
	getErr := c.Get(ctx, client.ObjectKey{Name: name}, &existing)
	if apierrors.IsNotFound(getErr) {
		return createNamespace(ctx, c, name, allOnCreate, meshLabels)
	}
	if getErr != nil {
		return "", fmt.Errorf("inspect namespace %q: %w", name, getErr)
	}

	// Compute MESH labels that are missing — consent gate is only on
	// these. Bookkeeping labels can be patched silently.
	missingMesh := map[string]string{}
	for k, v := range meshLabels {
		if cur, ok := existing.Labels[k]; !ok || cur != v {
			missingMesh[k] = v
		}
	}
	missingBookkeeping := map[string]string{}
	for k, v := range managedLabel {
		if cur, ok := existing.Labels[k]; !ok || cur != v {
			missingBookkeeping[k] = v
		}
	}

	// Always-safe: silently patch bookkeeping labels.
	if len(missingBookkeeping) > 0 && len(missingMesh) == 0 {
		if err := patchLabels(ctx, c, &existing, missingBookkeeping); err != nil {
			return "", err
		}
		return "already configured (managed-by added)", nil
	}
	if len(missingMesh) == 0 && len(missingBookkeeping) == 0 {
		return "already configured", nil
	}

	// From here on missingMesh > 0 — gate by policy.
	switch policy {
	case OnExistingDelete:
		if err := c.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("delete existing namespace %q: %w", name, err)
		}
		if err := waitNamespaceGone(ctx, c, name, 60*time.Second); err != nil {
			return "", err
		}
		return createNamespace(ctx, c, name, allOnCreate, meshLabels)
	case OnExistingPatch:
		merged := mergeLabels(missingBookkeeping, missingMesh)
		if err := patchLabels(ctx, c, &existing, merged); err != nil {
			return "", err
		}
		return "labels patched: " + labelsString(missingMesh), nil
	default:
		return "", fmt.Errorf(
			"%w: namespace %q already exists and is missing mesh-required labels (%s); "+
				"these labels change dataplane behaviour for every Pod in the namespace, so "+
				"route-prism will not patch them silently. Re-run with --on-existing=delete to "+
				"wipe-and-recreate the namespace (DESTROYS any other resources in it), or "+
				"--on-existing=patch if you have already verified that no other workload shares this namespace",
			ErrNamespaceConsentRequired, name, labelsString(missingMesh))
	}
}

func patchLabels(ctx context.Context, c client.Client, ns *corev1.Namespace, add map[string]string) error {
	patch := client.MergeFrom(ns.DeepCopy())
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	maps.Copy(ns.Labels, add)
	if err := c.Patch(ctx, ns, patch); err != nil {
		return fmt.Errorf("patch namespace %q labels: %w", ns.Name, err)
	}
	return nil
}

// ErrNamespaceConsentRequired is returned by reconcileNamespace when the
// namespace already exists, mesh labels are missing, and the caller has
// not explicitly opted in to deletion or patching.
var ErrNamespaceConsentRequired = errors.New("namespace exists; explicit consent required")

func createNamespace(ctx context.Context, c client.Client, name string, allLabels, meshOnly map[string]string) (string, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: allLabels}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create namespace %q: %w", name, err)
	}
	if len(meshOnly) > 0 {
		return fmt.Sprintf("created with %s", labelsString(meshOnly)), nil
	}
	return "created", nil
}

func waitNamespaceGone(ctx context.Context, c client.Client, name string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var ns corev1.Namespace
		err := c.Get(ctx, client.ObjectKey{Name: name}, &ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for namespace %q deletion", name)
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
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
// KubeconfigPath and returns non-destructive observations: detected mesh
// info plus the state of the verify namespace (does it exist? what
// labels does it carry vs what we'd need to add?).
//
// The TUI uses this between context selection and the deep probe so it
// can (a) prompt for the Istio mode override and (b) ask for explicit
// consent before touching a pre-existing namespace.
func Inspect(ctx context.Context, opts Options) (Inspection, error) {
	if opts.Namespace == "" {
		opts.Namespace = VerifyNamespace
	}
	cfg, err := RestConfigFor(opts.KubeconfigPath, opts.ContextName)
	if err != nil {
		return Inspection{}, err
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return Inspection{}, fmt.Errorf("build client: %w", err)
	}
	mesh := preflight.DetectMesh(ctx, c)
	if opts.MeshOverride != "" && mesh.Kind == preflight.MeshIstio {
		mesh.Mode = opts.MeshOverride
	}

	// Only mesh-required labels gate the consent prompt; the bookkeeping
	// `app.kubernetes.io/managed-by` label is added unilaterally by
	// reconcileNamespace and never affects dataplane behaviour.
	meshRequired := mesh.NamespaceLabels()
	state := NamespaceState{}
	var existing corev1.Namespace
	getErr := c.Get(ctx, client.ObjectKey{Name: opts.Namespace}, &existing)
	if getErr == nil {
		state.Exists = true
		state.ExistingLabels = existing.Labels
		for k, v := range meshRequired {
			if cur, ok := existing.Labels[k]; !ok || cur != v {
				if state.MissingLabels == nil {
					state.MissingLabels = map[string]string{}
				}
				state.MissingLabels[k] = v
			}
		}
	} else if !apierrors.IsNotFound(getErr) {
		return Inspection{}, fmt.Errorf("inspect namespace %q: %w", opts.Namespace, getErr)
	}
	return Inspection{Mesh: mesh, Namespace: state}, nil
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
