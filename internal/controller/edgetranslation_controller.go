/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// EdgeTranslationReconciler reconciles an EdgeTranslation object.
type EdgeTranslationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=edgetranslations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=edgetranslations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=contextroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

func (r *EdgeTranslationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("edgetranslation", req.NamespacedName)

	var et routeprismv1alpha1.EdgeTranslation
	if err := r.Get(ctx, req.NamespacedName, &et); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if et.Spec.Mode != routeprismv1alpha1.EdgeModeRouter {
		setETCondition(&et, "Ready", metav1.ConditionFalse, "ModeNotImplemented",
			fmt.Sprintf("EdgeMode %q is not implemented in this prototype.", et.Spec.Mode))
		r.event(&et, corev1.EventTypeWarning, EventReasonModeNotImplemented,
			"EdgeMode %q is not implemented; only %q is supported.",
			et.Spec.Mode, routeprismv1alpha1.EdgeModeRouter)
		return ctrl.Result{}, r.Status().Update(ctx, &et)
	}

	targetName := et.Spec.Target.Service.Name
	et.Status.TargetService = targetName

	// 1) Resolve target Service.
	var target corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: et.Namespace, Name: targetName}, &target); err != nil {
		if apierrors.IsNotFound(err) {
			r.event(&et, corev1.EventTypeWarning, EventReasonTargetNotFound,
				"target Service %q not found in namespace %q", targetName, et.Namespace)
			setETCondition(&et, "Ready", metav1.ConditionFalse, "TargetNotFound", "target Service does not exist")
			return ctrl.Result{}, r.Status().Update(ctx, &et)
		}
		return ctrl.Result{}, err
	}
	if !hasAppProtocol(&target) {
		r.event(&et, corev1.EventTypeWarning, EventReasonMissingAppProtocol,
			"target Service %q has no port with appProtocol set; Cilium GAMMA requires appProtocol (e.g. \"http\") to program L7 dataplane.",
			target.Name)
	}

	// 2) Detect smart mode (a ContextRoute attaches to the same target).
	cr, err := r.findCRForTarget(ctx, et.Namespace, targetName)
	if err != nil {
		return ctrl.Result{}, err
	}
	smart := cr != nil

	// 3) DNS resolver.
	dnsIP, dnsErr := r.lookupClusterDNSIP(ctx)
	if dnsErr != nil {
		log.Info("could not detect cluster DNS Service ClusterIP, falling back", "err", dnsErr)
		r.event(&et, corev1.EventTypeWarning, EventReasonDNSLookupFailed,
			"Could not detect cluster DNS Service ClusterIP (%v); falling back to %s.",
			dnsErr, fallbackDNSClusterIP)
	}

	// 4) Render nginx.conf. Always bakes target Pod IP into the config so
	// the translator forwards directly to a Pod, bypassing Cilium GAMMA
	// on the outbound hop. In smart mode (CR also targeting same Service),
	// variant Pod IPs are added on top so the translator picks a variant
	// based on the cookie value.
	defaultIP, defaultPort, err := r.firstReadyEndpoint(ctx, et.Namespace, target.Name)
	if err != nil || defaultIP == "" {
		r.event(&et, corev1.EventTypeWarning, EventReasonTargetNotFound,
			"could not resolve any ready endpoint for target Service %q: %v",
			target.Name, err)
		setETCondition(&et, "Ready", metav1.ConditionFalse, "NoReadyEndpoints",
			"target Service has no ready endpoints yet")
		return ctrl.Result{}, r.Status().Update(ctx, &et)
	}
	var (
		variants   []VariantBackend
		routingKey string
	)
	if smart {
		variants, err = r.collectVariantBackends(ctx, cr, target.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		// In smart mode the matching ContextRoute is the source of truth
		// for the routing key — the translator only acts on the entry of
		// the cookie whose key matches it.
		routingKey = cr.EffectiveRoutingKey()
	} else {
		// No CR attached → no variants; routingKey is moot but we set a
		// sensible default ("<ns>.<target>") so the lua block has a
		// well-defined identity even when the cookie is empty.
		routingKey = et.Namespace + "." + target.Name
	}
	conf, err := renderNginxConf(&et, routingKey, dnsIP, defaultIP, defaultPort, variants)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5) Apply translator workload (ConfigMap, Service, Deployment with
	//    config-checksum annotation so changes roll out new pods).
	if err := r.apply(ctx, renderTranslatorConfigMap(&et, conf)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply ConfigMap: %w", err)
	}
	if err := r.apply(ctx, renderTranslatorService(&et)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Service: %w", err)
	}
	if err := r.apply(ctx, renderTranslatorDeployment(&et, conf)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Deployment: %w", err)
	}

	// 6) Apply HTTPRoute on target.
	if err := r.apply(ctx, renderETHTTPRoute(&et, &target)); err != nil {
		log.Error(err, "apply HTTPRoute")
		r.event(&et, corev1.EventTypeWarning, EventReasonHTTPRouteApplyFailed,
			"failed to apply HTTPRoute on target %q: %v", target.Name, err)
		return ctrl.Result{}, err
	}

	// 7) Status.
	var liveDep appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: et.Namespace, Name: translatorName(et.Name)}, &liveDep); err == nil {
		et.Status.TranslatorReady = liveDep.Status.AvailableReplicas >= 1
	} else {
		et.Status.TranslatorReady = false
	}

	if et.Status.TranslatorReady {
		modeStr := "standard"
		if smart {
			modeStr = "smart"
		}
		setETCondition(&et, "Ready", metav1.ConditionTrue, "Reconciled",
			fmt.Sprintf("Translator ready in %s mode; HTTPRoute applied.", modeStr))
		r.event(&et, corev1.EventTypeNormal, EventReasonReconciled,
			"Translator (%s mode) ready; applied HTTPRoute %q on target %q",
			modeStr, httpRouteNameForET(target.Name), target.Name)
	} else {
		setETCondition(&et, "Ready", metav1.ConditionFalse, "TranslatorNotReady",
			"Translator deployment has no available replicas yet.")
		r.event(&et, corev1.EventTypeWarning, EventReasonTranslatorNotReady,
			"Translator Deployment %q has no available replicas yet.", translatorName(et.Name))
	}
	if err := r.Status().Update(ctx, &et); err != nil {
		return ctrl.Result{}, err
	}

	r.surfaceHTTPRouteRejection(ctx, &et, target.Name)
	return ctrl.Result{}, nil
}

func (r *EdgeTranslationReconciler) apply(ctx context.Context, obj client.Object) error {
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldOwnerET), client.ForceOwnership)
}

// findCRForTarget returns a ContextRoute attached to the same target Service,
// or nil if none. If multiple match, returns the oldest (matches CR
// reconciler's first-match-wins semantic).
func (r *EdgeTranslationReconciler) findCRForTarget(ctx context.Context, ns, target string) (*routeprismv1alpha1.ContextRoute, error) {
	var list routeprismv1alpha1.ContextRouteList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var winner *routeprismv1alpha1.ContextRoute
	for i := range list.Items {
		c := &list.Items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if c.Spec.Target.Service.Name != target {
			continue
		}
		if winner == nil ||
			c.CreationTimestamp.Before(&winner.CreationTimestamp) ||
			(c.CreationTimestamp.Equal(&winner.CreationTimestamp) && c.Name < winner.Name) {
			winner = c
		}
	}
	return winner, nil
}

// collectVariantBackends builds the variant backend list for the smart-mode
// nginx config. For each Service matching the CR's variant selector
// (excluding the target itself), it picks the first ready endpoint.
func (r *EdgeTranslationReconciler) collectVariantBackends(ctx context.Context, cr *routeprismv1alpha1.ContextRoute, targetName string) ([]VariantBackend, error) {
	selector, err := metav1.LabelSelectorAsSelector(&cr.Spec.Variants.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid variants.selector: %w", err)
	}
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	out := make([]VariantBackend, 0, len(svcs.Items))
	for i := range svcs.Items {
		s := &svcs.Items[i]
		if s.Name == targetName {
			continue
		}
		ip, port, err := r.firstReadyEndpoint(ctx, s.Namespace, s.Name)
		if err != nil || ip == "" {
			// Skip variants without ready endpoints; they'll be picked
			// up on the next reconcile when their EndpointSlice changes.
			continue
		}
		out = append(out, VariantBackend{
			CookieValue: s.Name,
			PodIP:       ip,
			Port:        port,
		})
	}
	return out, nil
}

// firstReadyEndpoint returns one ready (address, port) tuple for the named
// Service. Picks the lexicographically first endpoint to be deterministic.
func (r *EdgeTranslationReconciler) firstReadyEndpoint(ctx context.Context, ns, svcName string) (string, int32, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(ns),
		client.MatchingLabels{discoveryv1.LabelServiceName: svcName},
	); err != nil {
		return "", 0, err
	}
	bestIP := ""
	var bestPort int32
	for _, sl := range slices.Items {
		if sl.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		// First port (matching our render assumption of single port).
		var port int32
		for _, p := range sl.Ports {
			if p.Port != nil {
				port = *p.Port
				break
			}
		}
		for _, ep := range sl.Endpoints {
			ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			if !ready {
				continue
			}
			for _, a := range ep.Addresses {
				if bestIP == "" || a < bestIP {
					bestIP = a
					bestPort = port
				}
			}
		}
	}
	if bestPort == 0 {
		bestPort = 80
	}
	return bestIP, bestPort, nil
}

func (r *EdgeTranslationReconciler) event(et *routeprismv1alpha1.EdgeTranslation, eventtype, reason, fmtStr string, args ...interface{}) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(et, eventtype, reason, fmtStr, args...)
}

func (r *EdgeTranslationReconciler) surfaceHTTPRouteRejection(ctx context.Context, et *routeprismv1alpha1.EdgeTranslation, targetName string) {
	var hr gwv1.HTTPRoute
	if err := r.Get(ctx, client.ObjectKey{Namespace: et.Namespace, Name: httpRouteNameForET(targetName)}, &hr); err != nil {
		return
	}
	for _, parent := range hr.Status.Parents {
		for _, cond := range parent.Conditions {
			rejected := cond.Status == metav1.ConditionFalse && (cond.Type == string(gwv1.RouteConditionAccepted) ||
				cond.Type == string(gwv1.RouteConditionResolvedRefs) ||
				cond.Type == "Programmed")
			if rejected {
				r.event(et, corev1.EventTypeWarning, EventReasonHTTPRouteRejected,
					"HTTPRoute %q (parent %s) — %s/%s by %q: %s",
					hr.Name, parent.ParentRef.Name,
					cond.Type, cond.Status,
					parent.ControllerName, cond.Message)
			}
		}
	}
}

func setETCondition(et *routeprismv1alpha1.EdgeTranslation, condType string, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: et.Generation,
	}
	out := et.Status.Conditions[:0]
	for _, c := range et.Status.Conditions {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	et.Status.Conditions = append(out, cond)
}

func (r *EdgeTranslationReconciler) lookupClusterDNSIP(ctx context.Context) (string, error) {
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kube-dns"}, &svc); err != nil {
		return "", err
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return "", fmt.Errorf("kube-dns Service has no ClusterIP (got %q)", svc.Spec.ClusterIP)
	}
	return svc.Spec.ClusterIP, nil
}

func (r *EdgeTranslationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapServiceToETs := func(ctx context.Context, obj client.Object) []reconcile.Request {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return nil
		}
		var list routeprismv1alpha1.EdgeTranslationList
		if err := r.List(ctx, &list, client.InNamespace(svc.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			et := &list.Items[i]
			if et.Spec.Target.Service.Name == svc.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: et.Namespace, Name: et.Name}})
				continue
			}
			// Smart mode: variant Service changes also affect us.
			cr, _ := r.findCRForTarget(ctx, et.Namespace, et.Spec.Target.Service.Name)
			if cr == nil {
				continue
			}
			sel, err := metav1.LabelSelectorAsSelector(&cr.Spec.Variants.Selector)
			if err != nil {
				continue
			}
			if sel.Matches(labels.Set(svc.Labels)) {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: et.Namespace, Name: et.Name}})
			}
		}
		return out
	}

	mapEndpointSliceToETs := func(ctx context.Context, obj client.Object) []reconcile.Request {
		es, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			return nil
		}
		svcName := es.Labels[discoveryv1.LabelServiceName]
		if svcName == "" {
			return nil
		}
		// Synthesize a Service object so we can reuse mapServiceToETs.
		fakeSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: es.Namespace,
			// Labels are not available on EndpointSlice → smart-mode
			// variant detection requires a fresh Service Get below.
		}}
		// Quickest path: enqueue all ETs in the namespace; reconciler
		// will short-circuit if irrelevant.
		_ = fakeSvc
		var list routeprismv1alpha1.EdgeTranslationList
		if err := r.List(ctx, &list, client.InNamespace(es.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
		}
		return out
	}

	mapCRToET := func(ctx context.Context, obj client.Object) []reconcile.Request {
		cr, ok := obj.(*routeprismv1alpha1.ContextRoute)
		if !ok {
			return nil
		}
		var list routeprismv1alpha1.EdgeTranslationList
		if err := r.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			et := &list.Items[i]
			if et.Spec.Target.Service.Name == cr.Spec.Target.Service.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: et.Namespace, Name: et.Name}})
			}
		}
		return out
	}

	mapHTTPRouteToET := func(_ context.Context, obj client.Object) []reconcile.Request {
		l := obj.GetLabels()
		if l[LabelManagedBy] != ManagedByValue || l[LabelKind] != KindEdgeTranslation {
			return nil
		}
		owner := l[LabelOwner]
		if owner == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&routeprismv1alpha1.EdgeTranslation{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(mapServiceToETs)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapEndpointSliceToETs)).
		Watches(&routeprismv1alpha1.ContextRoute{}, handler.EnqueueRequestsFromMapFunc(mapCRToET)).
		Watches(&gwv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(mapHTTPRouteToET)).
		Named("edgetranslation").
		Complete(r)
}
