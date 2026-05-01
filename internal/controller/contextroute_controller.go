/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// Event reasons. All abnormal conditions surface as Kubernetes Events on
// the originating CR. Use `kubectl events --for <kind>/<name>` to inspect.
const (
	EventReasonReconciled           = "Reconciled"
	EventReasonConflict             = "Conflict"
	EventReasonModeNotImplemented   = "ModeNotImplemented"
	EventReasonDNSLookupFailed      = "DNSLookupFailed"
	EventReasonTranslatorNotReady   = "TranslatorNotReady"
	EventReasonTargetNotFound       = "TargetNotFound"
	EventReasonInvalidSelector      = "InvalidSelector"
	EventReasonNoMatchingVariants   = "NoMatchingVariants"
	EventReasonMissingAppProtocol   = "MissingAppProtocol"
	EventReasonHTTPRouteApplyFailed = "HTTPRouteApplyFailed"
	EventReasonHTTPRouteRejected    = "HTTPRouteRejected"
	// EventReasonUnsupportedTargetType fires when the target Service's
	// spec.type is something other than ClusterIP (or unset, which
	// defaults to ClusterIP). GAMMA HTTPRoutes only attach to ClusterIP
	// Services on Cilium / Istio — NodePort, LoadBalancer, ExternalName
	// silently leave the HTTPRoute with an empty status and routing
	// falls through to the Service's plain selector.
	EventReasonUnsupportedTargetType = "UnsupportedTargetType"
)

// gammaSupportedServiceType returns true when svc.Spec.Type is a value
// that GAMMA-enabled meshes will attach an HTTPRoute to. ClusterIP (or
// the implicit default of an empty string) is the only currently
// supported parent kind.
func gammaSupportedServiceType(svc *corev1.Service) bool {
	switch svc.Spec.Type {
	case "", corev1.ServiceTypeClusterIP:
		return true
	default:
		return false
	}
}

// ContextRouteReconciler reconciles a ContextRoute object.
type ContextRouteReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=contextroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=contextroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=contextroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=edgetransformations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

//nolint:gocyclo // Reconcile naturally branches across resource phases.
func (r *ContextRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("contextroute", req.NamespacedName)

	var cr routeprismv1alpha1.ContextRoute
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !cr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cr, FinalizerCR) {
			if err := r.deleteOwnedHTTPRoute(ctx, &cr); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&cr, FinalizerCR)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&cr, FinalizerCR) {
		controllerutil.AddFinalizer(&cr, FinalizerCR)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	targetName := cr.Spec.Target.Service.Name
	cr.Status.TargetService = targetName

	// 1) Conflict resolution: only one ContextRoute per target Service.
	winner, err := r.resolveOwner(ctx, &cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if winner != nil && winner.Name != cr.Name {
		cr.Status.Conflict = &routeprismv1alpha1.ConflictRef{
			Service: targetName,
			OwnedBy: winner.Name,
		}
		cr.Status.MatchedVariants = 0
		cr.Status.VariantServices = nil
		setReadyCondition(&cr, false, "Conflict",
			fmt.Sprintf("ContextRoute %q owns target %q (older creationTimestamp).", winner.Name, targetName))
		r.event(&cr, corev1.EventTypeWarning, EventReasonConflict,
			"target %q is owned by ContextRoute %q (older creationTimestamp); skipping.",
			targetName, winner.Name)
		return ctrl.Result{}, r.Status().Update(ctx, &cr)
	}
	cr.Status.Conflict = nil

	// 1b) RoutingKey conflict — every ContextRoute's effective routing
	//     key (Spec.RoutingKey or "<ns>.<target>") must be cluster-unique.
	//     When two CRs collide we let the older creationTimestamp keep its
	//     key and mark the newer one Ready=False/RoutingKeyConflict.
	{
		myKey := cr.EffectiveRoutingKey()
		var allCRs routeprismv1alpha1.ContextRouteList
		if err := r.List(ctx, &allCRs); err != nil {
			return ctrl.Result{}, err
		}
		var keyOwner *routeprismv1alpha1.ContextRoute
		for i := range allCRs.Items {
			other := &allCRs.Items[i]
			if other.UID == cr.UID {
				continue
			}
			if other.EffectiveRoutingKey() != myKey {
				continue
			}
			// Older wins; ties broken by name for determinism.
			if keyOwner == nil ||
				other.CreationTimestamp.Before(&keyOwner.CreationTimestamp) ||
				(other.CreationTimestamp.Equal(&keyOwner.CreationTimestamp) && other.Name < keyOwner.Name) {
				keyOwner = other
			}
		}
		if keyOwner != nil &&
			(keyOwner.CreationTimestamp.Before(&cr.CreationTimestamp) ||
				(keyOwner.CreationTimestamp.Equal(&cr.CreationTimestamp) && keyOwner.Name < cr.Name)) {
			cr.Status.MatchedVariants = 0
			cr.Status.VariantServices = nil
			msg := fmt.Sprintf(
				"routingKey %q conflicts with ContextRoute %q in namespace %q (older creationTimestamp wins).",
				myKey, keyOwner.Name, keyOwner.Namespace)
			setReadyCondition(&cr, false, "RoutingKeyConflict", msg)
			r.event(&cr, corev1.EventTypeWarning, EventReasonConflict, msg)
			return ctrl.Result{}, r.Status().Update(ctx, &cr)
		}
	}

	// 2) Resolve target Service.
	var target corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: targetName}, &target); err != nil {
		if apierrors.IsNotFound(err) {
			r.event(&cr, corev1.EventTypeWarning, EventReasonTargetNotFound,
				"target Service %q not found in namespace %q", targetName, cr.Namespace)
			setReadyCondition(&cr, false, "TargetNotFound", "target Service does not exist")
			return ctrl.Result{}, r.Status().Update(ctx, &cr)
		}
		return ctrl.Result{}, err
	}

	if !hasAppProtocol(&target) {
		r.event(&cr, corev1.EventTypeWarning, EventReasonMissingAppProtocol,
			"target Service %q has no port with appProtocol set; Cilium GAMMA requires appProtocol (e.g. \"http\") to program L7 dataplane.",
			target.Name)
	}

	if !gammaSupportedServiceType(&target) {
		msg := fmt.Sprintf(
			"target Service %q has spec.type=%q; GAMMA HTTPRoutes attach only to ClusterIP Services, so cookie/baggage routing will silently fall through to the Service's plain selector",
			target.Name, target.Spec.Type)
		r.event(&cr, corev1.EventTypeWarning, EventReasonUnsupportedTargetType, "%s", msg)
		setReadyCondition(&cr, false, EventReasonUnsupportedTargetType, msg)
		return ctrl.Result{}, r.Status().Update(ctx, &cr)
	}

	// 3) Resolve variants.
	selector, err := metav1.LabelSelectorAsSelector(&cr.Spec.Variants.Selector)
	if err != nil {
		r.event(&cr, corev1.EventTypeWarning, EventReasonInvalidSelector,
			"spec.variants.selector is invalid: %v", err)
		return ctrl.Result{}, fmt.Errorf("invalid variants.selector: %w", err)
	}
	var svcList corev1.ServiceList
	if err := r.List(ctx, &svcList, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, err
	}
	// Exclude the target itself from variants — divert "to itself" is a no-op
	// rule and would shadow the catch-all.
	variants := make([]corev1.Service, 0, len(svcList.Items))
	for _, s := range svcList.Items {
		if s.Name == targetName {
			continue
		}
		variants = append(variants, s)
	}
	if len(variants) == 0 {
		r.event(&cr, corev1.EventTypeWarning, EventReasonNoMatchingVariants,
			"variant selector %q matched no Services (excluding the target).", selector.String())
	}

	// 4) Apply HTTPRoute — but only when no EdgeTransformation attaches to
	// the same target. When one does, the EdgeTransformation reconciler
	// renders a "smart" translator that does the variant routing itself
	// (forwarding to Pod IPs). In that case we MUST NOT also emit a
	// HTTPRoute here, because the translator would loop back through it.
	hasET, err := r.hasEdgeTransformationOnTarget(ctx, cr.Namespace, target.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasET {
		// Drop any HTTPRoute we previously owned (smart mode kicks in).
		if err := r.deleteOwnedHTTPRoute(ctx, &cr); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		hr := renderCRHTTPRoute(&cr, &target, variants, true)
		if err := r.Patch(ctx, hr, client.Apply, client.FieldOwner(FieldOwnerCR), client.ForceOwnership); err != nil { //nolint:staticcheck // client.Apply replacement requires server-side apply refactor; tracked separately.
			log.Error(err, "apply HTTPRoute")
			r.event(&cr, corev1.EventTypeWarning, EventReasonHTTPRouteApplyFailed,
				"failed to apply HTTPRoute on target %q: %v", target.Name, err)
			return ctrl.Result{}, err
		}
	}

	// 5) Status + events.
	variantNames := make([]string, len(variants))
	for i, v := range variants {
		variantNames[i] = v.Name
	}
	sort.Strings(variantNames)
	cr.Status.MatchedVariants = int32(len(variantNames))
	cr.Status.VariantServices = variantNames
	if hasET {
		setReadyCondition(&cr, true, "DelegatedToEdgeTransformation",
			"An EdgeTransformation owns routing for this target via smart-mode translator; no HTTPRoute emitted by ContextRoute.")
	} else {
		setReadyCondition(&cr, true, "Reconciled", "HTTPRoute applied.")
	}
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, err
	}

	if !hasET {
		r.surfaceHTTPRouteRejection(ctx, &cr, target.Name)
		r.event(&cr, corev1.EventTypeNormal, EventReasonReconciled,
			"Applied HTTPRoute %q on target %q with %d variant(s): %v",
			httpRouteNameForCR(target.Name), target.Name, len(variantNames), variantNames)
	} else {
		r.event(&cr, corev1.EventTypeNormal, EventReasonReconciled,
			"Routing delegated to EdgeTransformation on target %q (smart mode); %d variant(s) contributed: %v",
			target.Name, len(variantNames), variantNames)
	}
	return ctrl.Result{}, nil
}

// resolveOwner returns the ContextRoute that should own the target Service
// (oldest creationTimestamp; tie-break by name). Returns nil if the input
// CR is the only one targeting that Service.
func (r *ContextRouteReconciler) resolveOwner(ctx context.Context, cr *routeprismv1alpha1.ContextRoute) (*routeprismv1alpha1.ContextRoute, error) {
	var list routeprismv1alpha1.ContextRouteList
	if err := r.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
		return nil, err
	}
	target := cr.Spec.Target.Service.Name
	var candidates []routeprismv1alpha1.ContextRoute
	for i := range list.Items {
		c := &list.Items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if c.Spec.Target.Service.Name == target {
			candidates = append(candidates, *c)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		ti := candidates[i].CreationTimestamp.Time
		tj := candidates[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return candidates[i].Name < candidates[j].Name
	})
	return &candidates[0], nil
}

func (r *ContextRouteReconciler) hasEdgeTransformationOnTarget(ctx context.Context, ns, target string) (bool, error) {
	var list routeprismv1alpha1.EdgeTransformationList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return false, err
	}
	for i := range list.Items {
		et := &list.Items[i]
		if et.DeletionTimestamp.IsZero() && et.Spec.Target.Service.Name == target {
			return true, nil
		}
	}
	return false, nil
}

func (r *ContextRouteReconciler) deleteOwnedHTTPRoute(ctx context.Context, cr *routeprismv1alpha1.ContextRoute) error {
	if cr.Status.TargetService == "" {
		return nil
	}
	hr := &gwv1.HTTPRoute{}
	hr.Name = httpRouteNameForCR(cr.Status.TargetService)
	hr.Namespace = cr.Namespace
	if err := r.Delete(ctx, hr); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *ContextRouteReconciler) surfaceHTTPRouteRejection(ctx context.Context, cr *routeprismv1alpha1.ContextRoute, targetName string) {
	var hr gwv1.HTTPRoute
	if err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: httpRouteNameForCR(targetName)}, &hr); err != nil {
		return
	}
	for _, parent := range hr.Status.Parents {
		for _, cond := range parent.Conditions {
			rejected := cond.Status == metav1.ConditionFalse && (cond.Type == string(gwv1.RouteConditionAccepted) ||
				cond.Type == string(gwv1.RouteConditionResolvedRefs) ||
				cond.Type == "Programmed")
			if rejected {
				r.event(cr, corev1.EventTypeWarning, EventReasonHTTPRouteRejected,
					"HTTPRoute %q (parent %s) — %s/%s by %q: %s",
					hr.Name, parent.ParentRef.Name,
					cond.Type, cond.Status,
					parent.ControllerName, cond.Message)
			}
		}
	}
}

func (r *ContextRouteReconciler) event(cr *routeprismv1alpha1.ContextRoute, eventtype, reason, fmtStr string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(cr, nil, eventtype, reason, reason, fmtStr, args...)
}

// hasAppProtocol returns true if any port on svc has appProtocol set.
func hasAppProtocol(svc *corev1.Service) bool {
	for _, p := range svc.Spec.Ports {
		if p.AppProtocol != nil && *p.AppProtocol != "" {
			return true
		}
	}
	return false
}

func setReadyCondition(cr *routeprismv1alpha1.ContextRoute, ready bool, reason, msg string) {
	cond := metav1.Condition{
		Type:               "Ready",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            msg,
	}
	if ready {
		cond.Status = metav1.ConditionTrue
	} else {
		cond.Status = metav1.ConditionFalse
	}
	out := cr.Status.Conditions[:0]
	for _, c := range cr.Status.Conditions {
		if c.Type != "Ready" {
			out = append(out, c)
		}
	}
	cr.Status.Conditions = append(out, cond)
}

func (r *ContextRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapServiceToCRs := func(ctx context.Context, obj client.Object) []reconcile.Request {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return nil
		}
		var list routeprismv1alpha1.ContextRouteList
		if err := r.List(ctx, &list, client.InNamespace(svc.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			cr := &list.Items[i]
			// Service is the target → always enqueue.
			if cr.Spec.Target.Service.Name == svc.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}})
				continue
			}
			// Service matches the variants selector → enqueue (rule list change).
			sel, err := metav1.LabelSelectorAsSelector(&cr.Spec.Variants.Selector)
			if err != nil {
				continue
			}
			if sel.Matches(labels.Set(svc.Labels)) {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}})
				continue
			}
			// Previously contributed as variant — also requeue (cleanup case).
			if slices.Contains(cr.Status.VariantServices, svc.Name) {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}})
			}
		}
		return out
	}

	// EdgeTransformation on the same target affects whether CR includes its
	// catch-all rule, so requeue affected CRs when ETs change.
	mapETToCR := func(ctx context.Context, obj client.Object) []reconcile.Request {
		et, ok := obj.(*routeprismv1alpha1.EdgeTransformation)
		if !ok {
			return nil
		}
		var list routeprismv1alpha1.ContextRouteList
		if err := r.List(ctx, &list, client.InNamespace(et.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			cr := &list.Items[i]
			if cr.Spec.Target.Service.Name == et.Spec.Target.Service.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}})
			}
		}
		return out
	}

	mapHTTPRouteToCR := func(_ context.Context, obj client.Object) []reconcile.Request {
		l := obj.GetLabels()
		if l[LabelManagedBy] != ManagedByValue || l[LabelKind] != KindContextRoute {
			return nil
		}
		owner := l[LabelOwner]
		if owner == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&routeprismv1alpha1.ContextRoute{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(mapServiceToCRs), builder.WithPredicates()).
		Watches(&routeprismv1alpha1.EdgeTransformation{}, handler.EnqueueRequestsFromMapFunc(mapETToCR), builder.WithPredicates()).
		Watches(&gwv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(mapHTTPRouteToCR), builder.WithPredicates()).
		Named("contextroute").
		Complete(r)
}
