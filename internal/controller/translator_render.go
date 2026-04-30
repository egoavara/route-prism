/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

//go:embed templates/nginx.smart.conf.tmpl
var nginxSmartTemplate string

var nginxSmartTmpl = template.Must(template.New("nginx-smart").Parse(nginxSmartTemplate))

// fallbackDNSClusterIP is the kubeadm/kind default kube-dns ClusterIP. Used
// only when the controller cannot detect the live ClusterIP at reconcile
// time.
const fallbackDNSClusterIP = "10.96.0.10"

// VariantBackend describes one variant in smart-mode nginx config.
type VariantBackend struct {
	CookieValue string // matched cookie value (typically the variant Service name)
	PodIP       string // current Pod IP backing the variant
	Port        int32  // upstream port
}

// nginxConfData carries everything the smart-mode template needs.
//
// RoutingKey is the per-CR routing key (default "<ns>.<svc>"). It serves
// two roles in the rendered nginx config:
//
//   1. Cookie parsing: the translator extracts the entry whose key equals
//      RoutingKey from the multi-key cookie value
//      "k1:v1|k2:v2|...". Other entries are ignored by *this* translator
//      (they're meant for sibling translators on other tiers).
//
//   2. Baggage emission: every non-`.` entry observed in the cookie is
//      replayed verbatim into the W3C Baggage header so downstream tiers
//      can use either the cookie or the baggage as the source of truth.
type nginxConfData struct {
	// Both keys are emitted into the lua block as raw strings. Lua does
	// equality on them — no regex involvement — so we don't escape.
	RoutingKey   string
	SourceCookie string
	Namespace    string
	DNSClusterIP string

	// Smart-mode forwarding targets.
	DefaultPodIP string
	DefaultPort  int32
	Variants     []VariantBackend
}

// renderNginxConf produces the translator nginx config. The translator
// always forwards directly to a Pod IP (no mesh second hop) so it never
// re-enters the target Service's HTTPRoute — that's why we no longer
// stamp a "checked" loop-prevention marker. When no ContextRoute attaches
// to the target, variants is empty and every request goes to defaultPodIP.
func renderNginxConf(et *routeprismv1alpha1.EdgeTranslation, routingKey, dnsClusterIP, defaultPodIP string, defaultPort int32, variants []VariantBackend) (string, error) {
	if dnsClusterIP == "" {
		dnsClusterIP = fallbackDNSClusterIP
	}
	// Sort variants for deterministic output.
	sorted := make([]VariantBackend, len(variants))
	copy(sorted, variants)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CookieValue < sorted[j].CookieValue })
	data := nginxConfData{
		RoutingKey:   routingKey,
		SourceCookie: et.Spec.SourceCookie,
		Namespace:    et.Namespace,
		DNSClusterIP: dnsClusterIP,
		DefaultPodIP: defaultPodIP,
		DefaultPort:  defaultPort,
		Variants:     sorted,
	}
	var buf bytes.Buffer
	if err := nginxSmartTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nginx smart: %w", err)
	}
	return buf.String(), nil
}

func translatorLabels(et *routeprismv1alpha1.EdgeTranslation) map[string]string {
	return map[string]string{
		LabelManagedBy:               ManagedByValue,
		LabelKind:                    KindEdgeTranslation,
		LabelOwner:                   et.Name,
		"app.kubernetes.io/name":     "route-prism-translator",
		"app.kubernetes.io/instance": et.Name,
	}
}

func ownerRefForET(et *routeprismv1alpha1.EdgeTranslation) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         routeprismv1alpha1.GroupVersion.String(),
		Kind:               "EdgeTranslation",
		Name:               et.Name,
		UID:                et.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(true),
	}
}

func renderTranslatorConfigMap(et *routeprismv1alpha1.EdgeTranslation, conf string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            translatorName(et.Name),
			Namespace:       et.Namespace,
			Labels:          translatorLabels(et),
			OwnerReferences: []metav1.OwnerReference{ownerRefForET(et)},
		},
		Data: map[string]string{"nginx.conf": conf},
	}
}

func renderTranslatorService(et *routeprismv1alpha1.EdgeTranslation) *corev1.Service {
	labels := translatorLabels(et)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            translatorName(et.Name),
			Namespace:       et.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRefForET(et)},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:        "http",
				Port:        80,
				TargetPort:  intstr.FromInt(80),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: ptr("http"),
			}},
		},
	}
}

// configChecksum returns a short stable hash of the rendered nginx.conf,
// stamped on the Deployment's pod template so kubelet rolls pods on
// changes (subPath ConfigMap mounts don't auto-update).
func configChecksum(conf string) string {
	sum := sha256.Sum256([]byte(conf))
	return hex.EncodeToString(sum[:8])
}

func renderTranslatorDeployment(et *routeprismv1alpha1.EdgeTranslation, conf string) *appsv1.Deployment {
	labels := translatorLabels(et)
	replicas := int32(1)
	podLabels := make(map[string]string, len(labels))
	for k, v := range labels {
		podLabels[k] = v
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            translatorName(et.Name),
			Namespace:       et.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRefForET(et)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
					Annotations: map[string]string{
						"route-prism.egoavara.net/config-checksum": configChecksum(conf),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "openresty",
						Image: "openresty/openresty:1.25.3.1-alpine",
						Ports: []corev1.ContainerPort{{ContainerPort: 80}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "conf",
							MountPath: "/usr/local/openresty/nginx/conf/nginx.conf",
							SubPath:   "nginx.conf",
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(80)},
							},
							PeriodSeconds: 5,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "conf",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: translatorName(et.Name)},
							},
						},
					}},
				},
			},
		},
	}
}
