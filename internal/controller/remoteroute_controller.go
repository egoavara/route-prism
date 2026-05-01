/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// RemoteRoute-specific event reasons.
const (
	EventReasonRRContextRouteNotFound = "ContextRouteNotFound"
	EventReasonRRUnsupportedSelector  = "UnsupportedSelector"
	EventReasonRRMixedScheme          = "MixedScheme"
	EventReasonRRIncompatibleLBModes  = "IncompatibleLBModes"
	EventReasonRRInvalidUpstream      = "InvalidUpstream"
	EventReasonRRProxyNotReady        = "ProxyNotReady"
	EventReasonRRAllUpstreamsDown     = "AllUpstreamsUnhealthy"
)

// rrAdminPollInterval is how often the controller re-fetches Envoy
// /clusters to refresh per-upstream health in status.
const rrAdminPollInterval = 15 * time.Second

// RemoteRouteReconciler reconciles a RemoteRoute object.
type RemoteRouteReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// AdminClient is the http.Client used to scrape the Envoy admin
	// endpoint. Defaults to a 2s-timeout client when nil.
	AdminClient *http.Client
}

// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=remoteroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=remoteroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=remoteroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=route-prism.egoavara.net,resources=contextroutes,verbs=get;list;watch

func (r *RemoteRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("remoteroute", req.NamespacedName)

	var rr routeprismv1alpha1.RemoteRoute
	if err := r.Get(ctx, req.NamespacedName, &rr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	rr.Status.VariantName = rr.Name

	// 1) Resolve parent ContextRoute and install ownerReference.
	var cr routeprismv1alpha1.ContextRoute
	if err := r.Get(ctx, client.ObjectKey{Namespace: rr.Namespace, Name: rr.Spec.ContextRouteRef.Name}, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			r.event(&rr, corev1.EventTypeWarning, EventReasonRRContextRouteNotFound,
				"ContextRoute %q not found in namespace %q", rr.Spec.ContextRouteRef.Name, rr.Namespace)
			setRRCondition(&rr, "Ready", metav1.ConditionFalse, "ContextRouteNotFound",
				fmt.Sprintf("ContextRoute %q does not exist", rr.Spec.ContextRouteRef.Name))
			return ctrl.Result{}, r.Status().Update(ctx, &rr)
		}
		return ctrl.Result{}, err
	}
	if changed := ensureRROwnerRef(&rr, &cr); changed {
		if err := r.Update(ctx, &rr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 2) Pull variant matchLabels from the parent CR. matchExpressions are
	//    not supported (we cannot synthesize labels guaranteed to match).
	if len(cr.Spec.Variants.Selector.MatchExpressions) > 0 {
		r.event(&rr, corev1.EventTypeWarning, EventReasonRRUnsupportedSelector,
			"ContextRoute %q variants selector uses matchExpressions; RemoteRoute supports matchLabels only",
			cr.Name)
		setRRCondition(&rr, "Ready", metav1.ConditionFalse, "UnsupportedSelector",
			"parent ContextRoute uses matchExpressions; switch to matchLabels only")
		return ctrl.Result{}, r.Status().Update(ctx, &rr)
	}
	variantLabels := cr.Spec.Variants.Selector.MatchLabels
	if len(variantLabels) == 0 {
		r.event(&rr, corev1.EventTypeWarning, EventReasonRRUnsupportedSelector,
			"ContextRoute %q variants selector has no matchLabels; the proxy Service would not be picked up as a variant",
			cr.Name)
		setRRCondition(&rr, "Ready", metav1.ConditionFalse, "UnsupportedSelector",
			"parent ContextRoute has empty matchLabels")
		return ctrl.Result{}, r.Status().Update(ctx, &rr)
	}

	// 3) Resolve upstreams → flat groups.
	groups, isHTTPS, lbPolicy, err := resolveUpstreams(&rr)
	if err != nil {
		reason := "InvalidUpstream"
		if strings.Contains(err.Error(), "mixed schemes") {
			reason = "MixedScheme"
			r.event(&rr, corev1.EventTypeWarning, EventReasonRRMixedScheme, "%s", err.Error())
		} else if strings.Contains(err.Error(), "incompatible group modes") {
			reason = "IncompatibleLBModes"
			r.event(&rr, corev1.EventTypeWarning, EventReasonRRIncompatibleLBModes, "%s", err.Error())
		} else {
			r.event(&rr, corev1.EventTypeWarning, EventReasonRRInvalidUpstream, "%s", err.Error())
		}
		setRRCondition(&rr, "Ready", metav1.ConditionFalse, reason, err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &rr)
	}

	// 4) Render envoy.yaml and apply workloads.
	conf, err := renderEnvoyConfig(&rr, groups, isHTTPS, lbPolicy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, renderRemoteConfigMap(&rr, conf)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply ConfigMap: %w", err)
	}
	if err := r.apply(ctx, renderRemoteService(&rr, variantLabels)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Service: %w", err)
	}
	if err := r.apply(ctx, renderRemoteAdminService(&rr)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply admin Service: %w", err)
	}
	if err := r.apply(ctx, renderRemoteDeployment(&rr, conf)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Deployment: %w", err)
	}

	// 5) ProxyReady from Deployment status.
	var dep appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: rr.Namespace, Name: remoteProxyName(rr.Name)}, &dep); err == nil {
		rr.Status.ProxyReady = dep.Status.AvailableReplicas >= 1
	} else {
		rr.Status.ProxyReady = false
	}

	// 6) Poll Envoy admin to update per-upstream health. Best-effort: a
	//    failure here keeps the previous status snapshot rather than
	//    erroring out the whole reconcile.
	if rr.Status.ProxyReady {
		r.refreshUpstreamHealth(ctx, &rr, groups)
	} else {
		// Proxy not up yet — surface "unknown" (Healthy=false) for all.
		rr.Status.Upstreams = initialUpstreamStatus(&rr, groups)
		rr.Status.ActiveUpstream = ""
	}

	// 7) Conditions + events.
	if !rr.Status.ProxyReady {
		setRRCondition(&rr, "Ready", metav1.ConditionFalse, "ProxyNotReady",
			"Envoy Deployment has no available replicas yet")
		r.event(&rr, corev1.EventTypeWarning, EventReasonRRProxyNotReady,
			"Envoy Deployment %q has no available replicas yet", remoteProxyName(rr.Name))
	} else {
		setRRCondition(&rr, "Ready", metav1.ConditionTrue, "Reconciled",
			fmt.Sprintf("Envoy proxy ready; %d upstream group(s) configured", len(groups)))
	}
	anyHealthy := false
	for _, u := range rr.Status.Upstreams {
		if u.Healthy {
			anyHealthy = true
			break
		}
	}
	if rr.Status.ProxyReady && !anyHealthy {
		setRRCondition(&rr, "UpstreamReachable", metav1.ConditionFalse, "AllUpstreamsUnhealthy",
			"no upstream is reachable; remote-routed traffic will return 5xx")
		r.event(&rr, corev1.EventTypeWarning, EventReasonRRAllUpstreamsDown,
			"All upstream groups are unhealthy; check the upstream PCs.")
	} else if rr.Status.ProxyReady {
		setRRCondition(&rr, "UpstreamReachable", metav1.ConditionTrue, "Reachable",
			fmt.Sprintf("active upstream: %s", rr.Status.ActiveUpstream))
	}

	if err := r.Status().Update(ctx, &rr); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("Reconciled RemoteRoute",
		"variant", rr.Name,
		"groups", len(groups),
		"active", rr.Status.ActiveUpstream,
		"proxyReady", rr.Status.ProxyReady)

	return ctrl.Result{RequeueAfter: rrAdminPollInterval}, nil
}

func (r *RemoteRouteReconciler) apply(ctx context.Context, obj client.Object) error {
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldOwnerRR), client.ForceOwnership) //nolint:staticcheck // client.Apply replacement requires server-side apply refactor; tracked separately.
}

// ensureRROwnerRef installs (and if needed updates) the controller
// ownerReference from RR → CR. Returns true when the object was mutated
// and needs Update().
func ensureRROwnerRef(rr *routeprismv1alpha1.RemoteRoute, cr *routeprismv1alpha1.ContextRoute) bool {
	want := metav1.OwnerReference{
		APIVersion:         routeprismv1alpha1.GroupVersion.String(),
		Kind:               "ContextRoute",
		Name:               cr.Name,
		UID:                cr.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(true),
	}
	for i, o := range rr.OwnerReferences {
		if o.UID == cr.UID {
			if ownerRefMatches(o, want) {
				return false
			}
			rr.OwnerReferences[i] = want
			return true
		}
	}
	rr.OwnerReferences = append(rr.OwnerReferences, want)
	return true
}

func ownerRefMatches(a, b metav1.OwnerReference) bool {
	if a.APIVersion != b.APIVersion || a.Kind != b.Kind || a.Name != b.Name || a.UID != b.UID {
		return false
	}
	return boolPtrEq(a.Controller, b.Controller) && boolPtrEq(a.BlockOwnerDeletion, b.BlockOwnerDeletion)
}

func boolPtrEq(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// envoyClusterDump is the slice of /clusters?format=json we care about.
type envoyClusterDump struct {
	ClusterStatuses []struct {
		Name         string `json:"name"`
		HostStatuses []struct {
			Address struct {
				SocketAddress struct {
					Address   string `json:"address"`
					PortValue int    `json:"port_value"`
				} `json:"socket_address"`
			} `json:"address"`
			HealthStatus struct {
				EdsHealthStatus  string `json:"eds_health_status,omitempty"`
				FailedActiveHC   bool   `json:"failed_active_health_check,omitempty"`
				FailedOutlier    bool   `json:"failed_outlier_check,omitempty"`
				FailedActiveDeg  bool   `json:"failed_active_degraded_check,omitempty"`
				PendingDynRemove bool   `json:"pending_dynamic_removal,omitempty"`
			} `json:"health_status"`
			Locality struct {
				Region  string `json:"region,omitempty"`
				Zone    string `json:"zone,omitempty"`
				SubZone string `json:"sub_zone,omitempty"`
			} `json:"locality,omitempty"`
			Priority int `json:"priority,omitempty"`
		} `json:"host_statuses"`
	} `json:"cluster_statuses"`
}

// refreshUpstreamHealth fetches /clusters from the proxy admin Service
// and rolls the result into rr.Status.Upstreams + ActiveUpstream.
//
// Best-effort: any error leaves the previous snapshot in place but marks
// every entry as not-yet-probed (LastProbeTime not advanced).
func (r *RemoteRouteReconciler) refreshUpstreamHealth(ctx context.Context, rr *routeprismv1alpha1.RemoteRoute, groups []envoyGroup) {
	statuses := initialUpstreamStatus(rr, groups)
	dump, err := r.fetchClusterDump(ctx, rr)
	if err == nil {
		applyDumpToStatus(statuses, dump)
		now := metav1.Now()
		for i := range statuses {
			statuses[i].LastProbeTime = &now
		}
	} else {
		for i := range statuses {
			statuses[i].Message = fmt.Sprintf("admin scrape failed: %v", err)
		}
	}
	rr.Status.Upstreams = statuses
	rr.Status.ActiveUpstream = ""
	for _, s := range statuses {
		if s.Healthy {
			rr.Status.ActiveUpstream = s.Name
			break
		}
	}
}

func (r *RemoteRouteReconciler) fetchClusterDump(ctx context.Context, rr *routeprismv1alpha1.RemoteRoute) (*envoyClusterDump, error) {
	cli := r.AdminClient
	if cli == nil {
		cli = &http.Client{Timeout: 2 * time.Second}
	}
	url := fmt.Sprintf("http://%s.%s.svc:9901/clusters?format=json",
		remoteProxyName(rr.Name)+"-admin", rr.Namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("admin returned %s", resp.Status)
	}
	var dump envoyClusterDump
	if err := json.NewDecoder(resp.Body).Decode(&dump); err != nil {
		return nil, err
	}
	return &dump, nil
}

// initialUpstreamStatus mirrors spec.upstreams into a status skeleton with
// every entry marked unhealthy. Filled in by applyDumpToStatus.
func initialUpstreamStatus(rr *routeprismv1alpha1.RemoteRoute, groups []envoyGroup) []routeprismv1alpha1.RemoteUpstreamStatus {
	out := make([]routeprismv1alpha1.RemoteUpstreamStatus, 0, len(rr.Spec.Upstreams))
	for i, u := range rr.Spec.Upstreams {
		var urls []string
		switch {
		case u.Group != nil:
			urls = append(urls, u.Group.URLs...)
		case u.URL != "":
			urls = []string{u.URL}
		}
		s := routeprismv1alpha1.RemoteUpstreamStatus{
			Name: u.Name,
			URLs: urls,
		}
		// In the rare case a future spec change pads groups with extra
		// metadata, defensively limit to known groups.
		_ = i
		_ = groups
		out = append(out, s)
	}
	return out
}

// applyDumpToStatus walks the cluster dump's "remote" cluster and counts
// per-priority healthy endpoints into the status slice.
func applyDumpToStatus(statuses []routeprismv1alpha1.RemoteUpstreamStatus, dump *envoyClusterDump) {
	if dump == nil {
		return
	}
	healthyByPrio := map[int]int{}
	totalByPrio := map[int]int{}
	for _, c := range dump.ClusterStatuses {
		if c.Name != "remote" {
			continue
		}
		for _, h := range c.HostStatuses {
			totalByPrio[h.Priority]++
			eds := h.HealthStatus.EdsHealthStatus
			healthy := !h.HealthStatus.FailedActiveHC &&
				!h.HealthStatus.FailedOutlier &&
				(eds == "" || eds == "HEALTHY")
			if healthy {
				healthyByPrio[h.Priority]++
			}
		}
	}
	for i := range statuses {
		statuses[i].HealthyEndpoints = int32(healthyByPrio[i])
		statuses[i].Healthy = healthyByPrio[i] > 0
		if totalByPrio[i] == 0 {
			statuses[i].Message = "no host_statuses reported by Envoy yet"
		} else if !statuses[i].Healthy {
			statuses[i].Message = "active health check failing"
		} else {
			statuses[i].Message = ""
		}
	}
	// Stable ordering for diff churn.
	sort.SliceStable(statuses, func(i, j int) bool { return i < j })
}

func setRRCondition(rr *routeprismv1alpha1.RemoteRoute, condType string, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: rr.Generation,
	}
	out := rr.Status.Conditions[:0]
	for _, c := range rr.Status.Conditions {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	rr.Status.Conditions = append(out, cond)
}

//nolint:unparam // signature kept for symmetry / future extensibility
func (r *RemoteRouteReconciler) event(rr *routeprismv1alpha1.RemoteRoute, eventtype, reason, fmtStr string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(rr, eventtype, reason, fmtStr, args...)
}

func (r *RemoteRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// When a ContextRoute changes (e.g. variants selector edited), every
	// RemoteRoute that points at it must reconcile so the proxy Service
	// labels stay in sync.
	mapCRToRRs := func(ctx context.Context, obj client.Object) []reconcile.Request {
		cr, ok := obj.(*routeprismv1alpha1.ContextRoute)
		if !ok {
			return nil
		}
		var list routeprismv1alpha1.RemoteRouteList
		if err := r.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			rr := &list.Items[i]
			if rr.Spec.ContextRouteRef.Name == cr.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: rr.Namespace, Name: rr.Name}})
			}
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&routeprismv1alpha1.RemoteRoute{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&routeprismv1alpha1.ContextRoute{}, handler.EnqueueRequestsFromMapFunc(mapCRToRRs)).
		Named("remoteroute").
		Complete(r)
}
