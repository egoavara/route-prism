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
	_ "embed"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

//go:embed templates/envoy.remote.yaml.tmpl
var envoyRemoteTemplate string

var envoyRemoteTmpl = template.Must(template.New("envoy-remote").Parse(envoyRemoteTemplate))

// envoyEndpoint is one resolved upstream socket address.
type envoyEndpoint struct {
	Host     string // IP literal or DNS name
	Port     int32
	Hostname string // SNI / Host header hint when Host is an IP
}

// envoyGroup carries one priority slot — single-URL entries collapse to a
// 1-endpoint group.
type envoyGroup struct {
	Name      string
	Endpoints []envoyEndpoint
}

// envoyHealthCheck flattens RemoteHealthCheck into the few fields the
// template actually reads.
type envoyHealthCheck struct {
	Type     string // "TCP" or "HTTP"
	HTTPPath string
	HTTPHost string
}

// envoyConfData is the rendered template's input.
type envoyConfData struct {
	LBPolicy           string // "ROUND_ROBIN" | "RANDOM"
	IsHTTPS            bool
	InsecureSkipVerify bool
	Groups             []envoyGroup
	HealthCheck        envoyHealthCheck
}

// resolveUpstreams collapses the spec's URL/Group entries into a flat
// priority-ordered list of envoy groups, plus derives the cluster-wide
// scheme and LB policy. Returns an error when any constraint is violated
// (mixed scheme, conflicting modes, malformed URL).
func resolveUpstreams(rr *routeprismv1alpha1.RemoteRoute) ([]envoyGroup, bool, string, error) {
	var (
		groups []envoyGroup
		scheme string
		modes  = map[routeprismv1alpha1.RemoteLBMode]struct{}{}
	)

	addURL := func(g *envoyGroup, raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("parse url %q: %w", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("unsupported scheme %q on %q", u.Scheme, raw)
		}
		if scheme == "" {
			scheme = u.Scheme
		} else if scheme != u.Scheme {
			return fmt.Errorf("mixed schemes (%s and %s) are not allowed in one RemoteRoute", scheme, u.Scheme)
		}
		host := u.Hostname()
		if host == "" {
			return fmt.Errorf("url %q has empty host", raw)
		}
		port, err := portForURL(u)
		if err != nil {
			return err
		}
		ep := envoyEndpoint{Host: host, Port: port}
		// When the host is a DNS name we set hostname: so Envoy can use
		// it for SNI on TLS upstreams.
		if u.Scheme == "https" && !looksLikeIP(host) {
			ep.Hostname = host
		}
		g.Endpoints = append(g.Endpoints, ep)
		return nil
	}

	for _, up := range rr.Spec.Upstreams {
		g := envoyGroup{Name: up.Name}
		switch {
		case up.Group != nil:
			modes[up.Group.Mode] = struct{}{}
			for _, raw := range up.Group.URLs {
				if err := addURL(&g, raw); err != nil {
					return nil, false, "", err
				}
			}
		case up.URL != "":
			if err := addURL(&g, up.URL); err != nil {
				return nil, false, "", err
			}
		default:
			return nil, false, "", fmt.Errorf("upstream %q has neither url nor group", up.Name)
		}
		groups = append(groups, g)
	}

	if scheme == "" {
		return nil, false, "", fmt.Errorf("no upstream URLs resolved")
	}

	mode := routeprismv1alpha1.RemoteLBRoundRobin
	if len(modes) > 1 {
		var seen []string
		for m := range modes {
			seen = append(seen, string(m))
		}
		return nil, false, "", fmt.Errorf("incompatible group modes (%s); all groups must declare the same mode", strings.Join(seen, ","))
	}
	for m := range modes {
		mode = m
	}
	lbPolicy := "ROUND_ROBIN"
	if mode == routeprismv1alpha1.RemoteLBRandom {
		lbPolicy = "RANDOM"
	}
	return groups, scheme == "https", lbPolicy, nil
}

func portForURL(u *url.URL) (int32, error) {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("invalid port in %q: %w", u.String(), err)
		}
		return int32(n), nil
	}
	switch u.Scheme {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	}
	return 0, fmt.Errorf("no default port for scheme %q", u.Scheme)
}

// looksLikeIP returns true for trivial IPv4 dotted-quads. Good enough for
// the SNI hint heuristic — false-negatives just mean we won't set
// hostname on something that happens to look like a hostname.
func looksLikeIP(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// renderEnvoyConfig produces the full envoy.yaml the proxy mounts.
func renderEnvoyConfig(rr *routeprismv1alpha1.RemoteRoute, groups []envoyGroup, isHTTPS bool, lbPolicy string) (string, error) {
	hc := envoyHealthCheck{Type: "TCP"}
	if rr.Spec.HealthCheck != nil && rr.Spec.HealthCheck.Type == routeprismv1alpha1.RemoteHealthCheckHTTP {
		hc.Type = "HTTP"
		if rr.Spec.HealthCheck.HTTP != nil {
			hc.HTTPPath = rr.Spec.HealthCheck.HTTP.Path
			hc.HTTPHost = rr.Spec.HealthCheck.HTTP.Host
		}
	}
	skip := false
	if rr.Spec.TLS != nil {
		skip = rr.Spec.TLS.InsecureSkipVerify
	}
	data := envoyConfData{
		LBPolicy:           lbPolicy,
		IsHTTPS:            isHTTPS,
		InsecureSkipVerify: skip,
		Groups:             groups,
		HealthCheck:        hc,
	}
	var buf bytes.Buffer
	if err := envoyRemoteTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render envoy.yaml: %w", err)
	}
	return buf.String(), nil
}

func ownerRefForRR(rr *routeprismv1alpha1.RemoteRoute) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         routeprismv1alpha1.GroupVersion.String(),
		Kind:               "RemoteRoute",
		Name:               rr.Name,
		UID:                rr.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(true),
	}
}

// rrIdentityLabels are stamped on every workload owned by a RemoteRoute
// (Deployment selector, ConfigMap label, internal Service selector).
// They are NOT the Service-level variant labels — those come from the
// parent ContextRoute's matchLabels.
func rrIdentityLabels(rr *routeprismv1alpha1.RemoteRoute) map[string]string {
	return map[string]string{
		LabelManagedBy:               ManagedByValue,
		LabelKind:                    KindRemoteRoute,
		LabelOwner:                   rr.Name,
		"app.kubernetes.io/name":     "route-prism-remote",
		"app.kubernetes.io/instance": rr.Name,
	}
}

// rrServiceLabels merges the variant matchLabels (so the proxy Service is
// auto-picked by ContextRoute's selector) with the identity labels (so
// the Service is discoverable by the operator).
func rrServiceLabels(rr *routeprismv1alpha1.RemoteRoute, variantLabels map[string]string) map[string]string {
	out := rrIdentityLabels(rr)
	for k, v := range variantLabels {
		out[k] = v
	}
	// metadata.name doubles as the variant key (cookie/baggage value);
	// stamp it as a Service label too so name-based listings work.
	out["app.kubernetes.io/name"] = rr.Name
	return out
}

func renderRemoteConfigMap(rr *routeprismv1alpha1.RemoteRoute, conf string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            remoteProxyName(rr.Name),
			Namespace:       rr.Namespace,
			Labels:          rrIdentityLabels(rr),
			OwnerReferences: []metav1.OwnerReference{ownerRefForRR(rr)},
		},
		Data: map[string]string{"envoy.yaml": conf},
	}
}

// renderRemoteService produces the user-facing Service. Selector targets
// the proxy Pods; metadata labels include the ContextRoute's variant
// matchLabels so the CR's HTTPRoute discovers it.
//
// The Service uses metadata.name = rr.Name so the variant key (which the
// CR controller uses verbatim as the Backend name in the Baggage match)
// matches the RemoteRoute name. That's what makes "RR name = cookie value"
// work end-to-end.
func renderRemoteService(rr *routeprismv1alpha1.RemoteRoute, variantLabels map[string]string) *corev1.Service {
	httpProto := "http"
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            rr.Name,
			Namespace:       rr.Namespace,
			Labels:          rrServiceLabels(rr, variantLabels),
			OwnerReferences: []metav1.OwnerReference{ownerRefForRR(rr)},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: rrIdentityLabels(rr),
			Ports: []corev1.ServicePort{{
				Name:        "http",
				Port:        80,
				TargetPort:  intstr.FromInt(8080),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: &httpProto,
			}},
		},
	}
}

// renderRemoteAdminService is an internal-only Service the controller
// uses to scrape Envoy admin /clusters. Kept separate from the data-plane
// Service so the variant Service surface stays clean (single port 80).
func renderRemoteAdminService(rr *routeprismv1alpha1.RemoteRoute) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            remoteProxyName(rr.Name) + "-admin",
			Namespace:       rr.Namespace,
			Labels:          rrIdentityLabels(rr),
			OwnerReferences: []metav1.OwnerReference{ownerRefForRR(rr)},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: rrIdentityLabels(rr),
			Ports: []corev1.ServicePort{{
				Name:       "admin",
				Port:       9901,
				TargetPort: intstr.FromInt(9902),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func renderRemoteDeployment(rr *routeprismv1alpha1.RemoteRoute, conf string) *appsv1.Deployment {
	labels := rrIdentityLabels(rr)
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            remoteProxyName(rr.Name),
			Namespace:       rr.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRefForRR(rr)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"route-prism.egoavara.net/config-checksum": configChecksum(conf),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "envoy",
						Image: "envoyproxy/envoy:v1.31-latest",
						Args: []string{
							"-c", "/etc/envoy/envoy.yaml",
							"--log-level", "warn",
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8080},
							{Name: "admin", ContainerPort: 9902},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("20m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "conf",
							MountPath: "/etc/envoy",
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8080)},
							},
							PeriodSeconds: 5,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8080)},
							},
							PeriodSeconds:    10,
							FailureThreshold: 3,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "conf",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: remoteProxyName(rr.Name)},
							},
						},
					}},
				},
			},
		},
	}
}
