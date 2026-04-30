/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"regexp"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// baggageMatchRegex matches Baggage values that carry the routing-key
// member "<routingKey>=<svc>" as a complete member (per W3C Baggage),
// bordered by member separators and not a substring of a longer name.
// routingKey defaults to "<ns>.<target-svc>" but is configurable per CR.
func baggageMatchRegex(routingKey, svcName string) string {
	return `(^|,)\s*` + regexp.QuoteMeta(routingKey) + `=` + regexp.QuoteMeta(svcName) + `(;[^,]*)?(\s*,|$)`
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
	return gwv1.PortNumber(svc.Spec.Ports[0].Port)
}

// sortedByName produces a stable, name-sorted copy of svcs. We render rules
// in deterministic order so SSA doesn't churn on every reconcile.
func sortedByName(svcs []corev1.Service) []corev1.Service {
	out := make([]corev1.Service, len(svcs))
	copy(out, svcs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderCRHTTPRoute produces a single HTTPRoute attached to the ContextRoute's
// target Service. The HTTPRoute contains:
//
//   - one rule per matched variant Service: when the Baggage carries
//     "x-route-prism=<variant-name>", route the request to that variant.
//   - if includeCatchAll, a final catch-all rule that sends unmarked traffic
//     back to the target Service. Set to false when an EdgeTranslation also
//     attaches to the target — its rules already provide the default path
//     and Cilium would otherwise merge two catch-alls into a 50/50 split.
func renderCRHTTPRoute(cr *routeprismv1alpha1.ContextRoute, target *corev1.Service, variants []corev1.Service, includeCatchAll bool) *gwv1.HTTPRoute {
	parentKind := gwv1.Kind("Service")
	coreGroup := gwv1.Group("")
	regexType := gwv1.HeaderMatchRegularExpression
	targetPort := firstPort(target)
	routingKey := cr.EffectiveRoutingKey()

	rules := make([]gwv1.HTTPRouteRule, 0, len(variants)+1)

	// Variant rules — deterministic order.
	for _, v := range sortedByName(variants) {
		vCopy := v
		vPort := firstPort(&vCopy)
		rules = append(rules, gwv1.HTTPRouteRule{
			Matches: []gwv1.HTTPRouteMatch{{
				Headers: []gwv1.HTTPHeaderMatch{{
					Type:  &regexType,
					Name:  gwv1.HTTPHeaderName(routeprismv1alpha1.PropagationHeader),
					Value: baggageMatchRegex(routingKey, vCopy.Name),
				}},
			}},
			BackendRefs: []gwv1.HTTPBackendRef{{
				BackendRef: gwv1.BackendRef{
					BackendObjectReference: gwv1.BackendObjectReference{
						Group: &coreGroup,
						Name:  gwv1.ObjectName(vCopy.Name),
						Port:  &vPort,
					},
				},
			}},
		})
	}

	if includeCatchAll {
		rules = append(rules, gwv1.HTTPRouteRule{
			BackendRefs: []gwv1.HTTPBackendRef{{
				BackendRef: gwv1.BackendRef{
					BackendObjectReference: gwv1.BackendObjectReference{
						Group: &coreGroup,
						Name:  gwv1.ObjectName(target.Name),
						Port:  &targetPort,
					},
				},
			}},
		})
	}

	return &gwv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gwv1.GroupVersion.String(),
			Kind:       "HTTPRoute",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      httpRouteNameForCR(target.Name),
			Namespace: target.Namespace,
			Labels:    httpRouteLabels(KindContextRoute, cr.Name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         routeprismv1alpha1.GroupVersion.String(),
				Kind:               "ContextRoute",
				Name:               cr.Name,
				UID:                cr.UID,
				Controller:         ptr(true),
				BlockOwnerDeletion: ptr(true),
			}},
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Group: &coreGroup,
					Kind:  &parentKind,
					Name:  gwv1.ObjectName(target.Name),
				}},
			},
			Rules: rules,
		},
	}
}

// renderETHTTPRoute produces the EdgeTranslation HTTPRoute attached to the
// target Service. The route is a single catch-all that sends every request
// to the translator. The translator forwards directly to a Pod IP (target
// or variant) so it never re-enters the mesh for the same target — no
// loop, no need for a "checked-marker" bypass rule.
func renderETHTTPRoute(et *routeprismv1alpha1.EdgeTranslation, target *corev1.Service) *gwv1.HTTPRoute {
	parentKind := gwv1.Kind("Service")
	coreGroup := gwv1.Group("")
	translatorPort := gwv1.PortNumber(80)

	return &gwv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gwv1.GroupVersion.String(),
			Kind:       "HTTPRoute",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      httpRouteNameForET(target.Name),
			Namespace: target.Namespace,
			Labels:    httpRouteLabels(KindEdgeTranslation, et.Name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         routeprismv1alpha1.GroupVersion.String(),
				Kind:               "EdgeTranslation",
				Name:               et.Name,
				UID:                et.UID,
				Controller:         ptr(true),
				BlockOwnerDeletion: ptr(true),
			}},
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Group: &coreGroup,
					Kind:  &parentKind,
					Name:  gwv1.ObjectName(target.Name),
				}},
			},
			Rules: []gwv1.HTTPRouteRule{{
				BackendRefs: []gwv1.HTTPBackendRef{{
					BackendRef: gwv1.BackendRef{
						BackendObjectReference: gwv1.BackendObjectReference{
							Group: &coreGroup,
							Name:  gwv1.ObjectName(translatorName(et.Name)),
							Port:  &translatorPort,
						},
					},
				}},
			}},
		},
	}
}

func ptr[T any](v T) *T { return &v }
