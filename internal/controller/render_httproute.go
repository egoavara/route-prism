/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"regexp"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// baggageMatchRegex matches Baggage values that carry the routing-key
// member "<routingKey>=<svc>" as a complete member (per W3C Baggage),
// bordered by member separators and not a substring of a longer name.
// routingKey defaults to "<ns>.<target-svc>" but is configurable per CR.
//
// Envoy (Cilium / Istio GAMMA) enforces FULL-string match on
// HTTPHeaderMatch RegularExpression — the regex must match the entire
// header value, not just a substring. So we anchor with optional prefix
// "(.*,)?" (everything up to and including a member separator) and an
// optional suffix "(,.*)?" (a member separator and the rest), instead of
// relying on partial-match boundaries. Without this wrapping a multi-
// member baggage like "a=x,foo=bar" silently fell back to the default
// rule under Cilium GAMMA.
func baggageMatchRegex(routingKey, svcName string) string {
	return `(?:.*,)?\s*` +
		regexp.QuoteMeta(routingKey) + `=` + regexp.QuoteMeta(svcName) +
		`(?:;[^,]*)?\s*(?:,.*)?`
}

func httpRouteLabels(kind, owner string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelKind:      kind,
		LabelOwner:     owner,
	}
}

func firstPort(svc *corev1.Service) gwv1.PortNumber {
	if len(svc.Spec.Ports) == 0 {
		return gwv1.PortNumber(80)
	}
	return svc.Spec.Ports[0].Port
}

// sortedByName produces a stable, name-sorted copy of svcs. We render rules
// in deterministic order so SSA doesn't churn on every reconcile.
func sortedByName(svcs []corev1.Service) []corev1.Service {
	out := make([]corev1.Service, len(svcs))
	copy(out, svcs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// crOwnerRef returns the controller OwnerReference apply-config for a CR.
func crOwnerRef(cr *routeprismv1alpha1.ContextRoute) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(routeprismv1alpha1.GroupVersion.String()).
		WithKind("ContextRoute").
		WithName(cr.Name).
		WithUID(cr.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

// etOwnerRef returns the controller OwnerReference apply-config for an ET.
func etOwnerRef(et *routeprismv1alpha1.EdgeTransformation) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(routeprismv1alpha1.GroupVersion.String()).
		WithKind("EdgeTransformation").
		WithName(et.Name).
		WithUID(et.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

// renderCRHTTPRoute produces a single HTTPRoute attached to the ContextRoute's
// target Service. The HTTPRoute contains:
//
//   - one rule per matched variant Service: when the Baggage carries
//     "x-route-prism=<variant-name>", route the request to that variant.
//   - if includeCatchAll, a final catch-all rule that sends unmarked traffic
//     back to the target Service. Set to false when an EdgeTransformation also
//     attaches to the target — its rules already provide the default path
//     and Cilium would otherwise merge two catch-alls into a 50/50 split.
func renderCRHTTPRoute(cr *routeprismv1alpha1.ContextRoute, target *corev1.Service, variants []corev1.Service, includeCatchAll bool) *gwv1ac.HTTPRouteApplyConfiguration {
	parentKind := gwv1.Kind("Service")
	coreGroup := gwv1.Group("")
	regexType := gwv1.HeaderMatchRegularExpression
	targetPort := firstPort(target)
	routingKey := cr.EffectiveRoutingKey()

	rules := make([]*gwv1ac.HTTPRouteRuleApplyConfiguration, 0, len(variants)+1)

	// Variant rules — deterministic order.
	for _, v := range sortedByName(variants) {
		vCopy := v
		vPort := firstPort(&vCopy)
		rules = append(rules, gwv1ac.HTTPRouteRule().
			WithMatches(gwv1ac.HTTPRouteMatch().
				WithHeaders(gwv1ac.HTTPHeaderMatch().
					WithType(regexType).
					WithName(gwv1.HTTPHeaderName(routeprismv1alpha1.PropagationHeader)).
					WithValue(baggageMatchRegex(routingKey, vCopy.Name)),
				),
			).
			WithBackendRefs(gwv1ac.HTTPBackendRef().
				WithGroup(coreGroup).
				WithName(gwv1.ObjectName(vCopy.Name)).
				WithPort(vPort),
			),
		)
	}

	if includeCatchAll {
		rules = append(rules, gwv1ac.HTTPRouteRule().
			WithBackendRefs(gwv1ac.HTTPBackendRef().
				WithGroup(coreGroup).
				WithName(gwv1.ObjectName(target.Name)).
				WithPort(targetPort),
			),
		)
	}

	return gwv1ac.HTTPRoute(httpRouteNameForCR(target.Name), target.Namespace).
		WithLabels(httpRouteLabels(KindContextRoute, cr.Name)).
		WithOwnerReferences(crOwnerRef(cr)).
		WithSpec(gwv1ac.HTTPRouteSpec().
			WithParentRefs(gwv1ac.ParentReference().
				WithGroup(coreGroup).
				WithKind(parentKind).
				WithName(gwv1.ObjectName(target.Name)),
			).
			WithRules(rules...),
		)
}

// renderETHTTPRoute produces the EdgeTransformation HTTPRoute attached to the
// target Service. The route is a single catch-all that sends every request
// to the translator. The translator forwards directly to a Pod IP (target
// or variant) so it never re-enters the mesh for the same target — no
// loop, no need for a "checked-marker" bypass rule.
func renderETHTTPRoute(et *routeprismv1alpha1.EdgeTransformation, target *corev1.Service) *gwv1ac.HTTPRouteApplyConfiguration {
	parentKind := gwv1.Kind("Service")
	coreGroup := gwv1.Group("")
	translatorPort := gwv1.PortNumber(80)

	return gwv1ac.HTTPRoute(httpRouteNameForET(target.Name), target.Namespace).
		WithLabels(httpRouteLabels(KindEdgeTransformation, et.Name)).
		WithOwnerReferences(etOwnerRef(et)).
		WithSpec(gwv1ac.HTTPRouteSpec().
			WithParentRefs(gwv1ac.ParentReference().
				WithGroup(coreGroup).
				WithKind(parentKind).
				WithName(gwv1.ObjectName(target.Name)),
			).
			WithRules(gwv1ac.HTTPRouteRule().
				WithBackendRefs(gwv1ac.HTTPBackendRef().
					WithGroup(coreGroup).
					WithName(gwv1.ObjectName(translatorName(et.Name))).
					WithPort(translatorPort),
				),
			),
		)
}

func ptr[T any](v T) *T { return &v }
