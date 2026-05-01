/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package preflight runs a one-shot startup check that the surrounding
// service mesh actually accepts HTTPRoute parentRefs targeting a Service
// (Gateway API GAMMA). The result is reported as a Kubernetes Event on the
// running Pod itself (via Downward API), so operators see it via
// `kubectl describe pod` / `kubectl get events`.
package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// Event reasons (PascalCase, k8s convention).
	ReasonGAMMASupported      = "MeshGAMMASupported"
	ReasonGAMMANotAccepted    = "MeshGAMMANotAccepted"
	ReasonGAMMANoController   = "MeshGAMMANoController"
	ReasonHTTPRouteCRDMissing = "HTTPRouteCRDMissing"
	ReasonProbeFailed         = "MeshProbeFailed"
	ReasonNoMeshDetected      = "NoMeshDetected"
	ReasonUnsupportedMeshVer  = "UnsupportedMeshVersion"
	ReasonExperimentalMeshVer = "ExperimentalMeshVersion"

	// Resource naming.
	probeNamePrefix = "route-prism-preflight-"

	// Defaults.
	defaultProbeTimeout = 30 * time.Second
	defaultPollInterval = 2 * time.Second
)

// Options configures preflight.Run.
type Options struct {
	// Namespace is the namespace to create the probe Service/HTTPRoute in.
	// Required; preflight is skipped if empty.
	Namespace string

	// SelfPod, if set, is used as the involvedObject for emitted events.
	// When nil, events are not emitted (logging only) — this is the
	// out-of-cluster / `make run` path where there is no Pod to attach to.
	SelfPod *corev1.ObjectReference

	// Timeout caps the live probe (waiting for HTTPRoute Accepted).
	// Defaults to 30s.
	Timeout time.Duration
}

// Runner implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable so
// it can be registered with mgr.Add. It runs once on Start and returns.
type Runner struct {
	cfg      *rest.Config
	scheme   *runtime.Scheme
	recorder record.EventRecorder
	opts     Options
}

// New constructs a Runner. The recorder is consulted for events when
// opts.SelfPod is set.
func New(cfg *rest.Config, scheme *runtime.Scheme, recorder record.EventRecorder, opts Options) *Runner {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultProbeTimeout
	}
	return &Runner{cfg: cfg, scheme: scheme, recorder: recorder, opts: opts}
}

// NeedLeaderElection returns true so the probe runs at most once per
// cluster (only on the elected leader). The check is a *capability*
// verification — not a per-Pod health signal — so duplicating it across
// replicas would only churn dummy resources and emit redundant events.
//
// When the manager runs without leader election (e.g. `make run`,
// single-replica dev), controller-runtime treats the process as the
// leader and the Runnable still executes.
func (*Runner) NeedLeaderElection() bool { return true }

// Start runs the preflight check synchronously, then returns. It never
// returns a non-nil error: preflight is informational and must not block
// manager startup.
func (r *Runner) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("preflight")

	if r.opts.Namespace == "" {
		log.Info("namespace unset (likely out-of-cluster run); skipping preflight")
		return nil
	}

	c, err := client.New(r.cfg, client.Options{Scheme: r.scheme})
	if err != nil {
		log.Error(err, "could not build preflight client")
		return nil
	}

	mesh := DetectMesh(ctx, c)
	log.Info("mesh detection", "summary", mesh.Summary())

	probeCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	res := r.liveProbe(probeCtx, c)

	r.report(log, mesh, res)
	return nil
}

// probeResult captures what the live probe observed.
type probeResult struct {
	// crdMissing is true if HTTPRoute CRD is not installed at all
	// (creation failed with NoKindMatchError).
	crdMissing bool
	// createErr is any non-NoKindMatch error from create. Surfaces e.g.
	// admission webhook rejections (some meshes may reject Service parentRefs).
	createErr error
	// accepted is true if any parent reported Accepted=True before timeout.
	accepted bool
	// controllerName is the controller that wrote the parent status, if any.
	controllerName string
	// reason and message are from the most relevant parent condition
	// (Accepted=False or Accepted=True; falls back to other conds).
	reason  string
	message string
	// timedOut indicates no parent status appeared at all within the
	// timeout window — usually means no controller picked up the route,
	// i.e. no GAMMA-aware mesh is installed.
	timedOut bool
}

func (r *Runner) liveProbe(ctx context.Context, c client.Client) probeResult {
	log := logf.FromContext(ctx).WithName("preflight")
	name := probeNamePrefix + randomSuffix(8)
	ns := r.opts.Namespace

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "route-prism",
				"app.kubernetes.io/component":  "preflight",
			},
			OwnerReferences: r.ownerRefs(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:        "http",
				Port:        80,
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: ptr.To("http"),
			}},
			Selector: map[string]string{
				"route-prism.egoavara.net/preflight": "intentionally-unmatched",
			},
		},
	}
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			Labels:          svc.Labels,
			OwnerReferences: r.ownerRefs(),
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Group: ptr.To(gwv1.Group("")),
					Kind:  ptr.To(gwv1.Kind("Service")),
					Name:  gwv1.ObjectName(name),
				}},
			},
			Rules: []gwv1.HTTPRouteRule{{
				BackendRefs: []gwv1.HTTPBackendRef{{
					BackendRef: gwv1.BackendRef{
						BackendObjectReference: gwv1.BackendObjectReference{
							Name: gwv1.ObjectName(name),
							Port: ptr.To(gwv1.PortNumber(80)),
						},
					},
				}},
			}},
		},
	}

	// Always cleanup, even on partial failure. Use a detached context so
	// cleanup still runs if the parent ctx (Start) is cancelled.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.Delete(cleanupCtx, hr)
		_ = c.Delete(cleanupCtx, svc)
	}()

	if err := c.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		log.Error(err, "preflight: create probe Service failed")
		return probeResult{createErr: fmt.Errorf("create probe Service: %w", err)}
	}

	if err := c.Create(ctx, hr); err != nil {
		if meta.IsNoMatchError(err) {
			return probeResult{crdMissing: true}
		}
		if !apierrors.IsAlreadyExists(err) {
			log.Error(err, "preflight: create probe HTTPRoute failed")
			return probeResult{createErr: fmt.Errorf("create probe HTTPRoute: %w", err)}
		}
	}

	// Poll for Accepted condition on any parent.
	res := probeResult{timedOut: true}
	_ = wait.PollUntilContextCancel(ctx, defaultPollInterval, true, func(ctx context.Context) (bool, error) {
		var current gwv1.HTTPRoute
		if err := c.Get(ctx, client.ObjectKeyFromObject(hr), &current); err != nil {
			return false, nil
		}
		if len(current.Status.Parents) == 0 {
			return false, nil
		}
		// Pick the first parent that has any conditions set.
		for _, p := range current.Status.Parents {
			if len(p.Conditions) == 0 {
				continue
			}
			res.timedOut = false
			res.controllerName = string(p.ControllerName)
			for _, cond := range p.Conditions {
				if cond.Type == string(gwv1.RouteConditionAccepted) {
					res.accepted = cond.Status == metav1.ConditionTrue
					res.reason = cond.Reason
					res.message = cond.Message
					return true, nil
				}
			}
			// No Accepted condition yet but other conds exist; keep waiting
			// briefly (other controllers may still write Accepted).
		}
		return false, nil
	})

	return res
}

func (r *Runner) ownerRefs() []metav1.OwnerReference {
	if r.opts.SelfPod == nil || r.opts.SelfPod.UID == "" {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       r.opts.SelfPod.Name,
		UID:        r.opts.SelfPod.UID,
	}}
}

func (r *Runner) report(log logr.Logger, mesh MeshInfo, res probeResult) {
	podStub := r.podStubForEvent()
	emit := func(eventType, reason, msgFmt string, args ...any) {
		msg := fmt.Sprintf(msgFmt, args...)
		log.Info("preflight result", "type", eventType, "reason", reason, "message", msg)
		if r.recorder == nil || podStub == nil {
			return
		}
		r.recorder.Event(podStub, eventType, reason, msg)
	}

	switch {
	case res.crdMissing:
		emit(corev1.EventTypeWarning, ReasonHTTPRouteCRDMissing,
			"Gateway API HTTPRoute CRD is not installed. Install Gateway API CRDs (sigs.k8s.io/gateway-api) before running route-prism. mesh=%s",
			mesh.Summary())
		return
	case res.createErr != nil:
		emit(corev1.EventTypeWarning, ReasonProbeFailed,
			"preflight could not create probe HTTPRoute: %v. mesh=%s",
			res.createErr, mesh.Summary())
		return
	}

	// Cross-check version matrix against detected mesh — even if probe
	// said accepted, a known-buggy version is worth surfacing.
	if issue := mesh.KnownIssue(); issue != "" {
		reason := ReasonUnsupportedMeshVer
		if mesh.Experimental() {
			reason = ReasonExperimentalMeshVer
		}
		emit(corev1.EventTypeWarning, reason,
			"%s probe accepted=%v controller=%q",
			issue, res.accepted, res.controllerName)
	}

	switch {
	case res.timedOut:
		emit(corev1.EventTypeWarning, ReasonGAMMANoController,
			"no controller accepted HTTPRoute with Service parentRef within %s — no GAMMA-capable mesh appears to be running. mesh=%s",
			r.opts.Timeout, mesh.Summary())
	case !res.accepted:
		emit(corev1.EventTypeWarning, ReasonGAMMANotAccepted,
			"controller %q did not accept HTTPRoute Service parentRef (reason=%s, message=%s). mesh=%s",
			res.controllerName, res.reason, res.message, mesh.Summary())
	default:
		if mesh.Kind == MeshUnknown {
			emit(corev1.EventTypeNormal, ReasonGAMMASupported,
				"GAMMA preflight OK (controller=%q) but the underlying mesh implementation could not be identified from istio-system/kube-system.",
				res.controllerName)
		} else {
			emit(corev1.EventTypeNormal, ReasonGAMMASupported,
				"GAMMA preflight OK. controller=%q mesh=%s",
				res.controllerName, mesh.Summary())
		}
	}
}

// podStubForEvent builds a minimal *corev1.Pod that the EventRecorder can
// resolve to an involvedObject. Returns nil when SelfPod is not configured.
func (r *Runner) podStubForEvent() *corev1.Pod {
	ref := r.opts.SelfPod
	if ref == nil || ref.Name == "" || ref.Namespace == "" {
		return nil
	}
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Name,
			Namespace: ref.Namespace,
			UID:       ref.UID,
		},
	}
}

func randomSuffix(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is exceptional; fall back to a fixed suffix
		// so the probe still runs (uniqueness only matters if multiple
		// preflight Runners race in the same namespace).
		return "fallback"
	}
	return hex.EncodeToString(b)[:n]
}
