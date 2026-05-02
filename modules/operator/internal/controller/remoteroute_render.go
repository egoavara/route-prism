/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
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
		if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
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
		if u.Scheme == schemeHTTPS && !looksLikeIP(host) {
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
	return groups, scheme == schemeHTTPS, lbPolicy, nil
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
	case schemeHTTP:
		return 80, nil
	case schemeHTTPS:
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

func ownerRefForRR(rr *routeprismv1alpha1.RemoteRoute) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(routeprismv1alpha1.GroupVersion.String()).
		WithKind("RemoteRoute").
		WithName(rr.Name).
		WithUID(rr.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
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
	maps.Copy(out, variantLabels)
	// metadata.name doubles as the variant key (cookie/baggage value);
	// stamp it as a Service label too so name-based listings work.
	out["app.kubernetes.io/name"] = rr.Name
	return out
}

func renderRemoteConfigMap(rr *routeprismv1alpha1.RemoteRoute, conf string) *corev1ac.ConfigMapApplyConfiguration {
	return corev1ac.ConfigMap(remoteProxyName(rr.Name), rr.Namespace).
		WithLabels(rrIdentityLabels(rr)).
		WithOwnerReferences(ownerRefForRR(rr)).
		WithData(map[string]string{"envoy.yaml": conf})
}

// renderRemoteService produces the user-facing Service. Selector targets
// the proxy Pods; metadata labels include the ContextRoute's variant
// matchLabels so the CR's HTTPRoute discovers it.
//
// The Service uses metadata.name = rr.Name so the variant key (which the
// CR controller uses verbatim as the Backend name in the Baggage match)
// matches the RemoteRoute name. That's what makes "RR name = cookie value"
// work end-to-end.
func renderRemoteService(rr *routeprismv1alpha1.RemoteRoute, variantLabels map[string]string) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(rr.Name, rr.Namespace).
		WithLabels(rrServiceLabels(rr, variantLabels)).
		WithOwnerReferences(ownerRefForRR(rr)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(rrIdentityLabels(rr)).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(80).
				WithTargetPort(intstr.FromInt(8080)).
				WithProtocol(corev1.ProtocolTCP).
				WithAppProtocol(schemeHTTP),
			),
		)
}

// renderRemoteAdminService is an internal-only Service the controller
// uses to scrape Envoy admin /clusters. Kept separate from the data-plane
// Service so the variant Service surface stays clean (single port 80).
func renderRemoteAdminService(rr *routeprismv1alpha1.RemoteRoute) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(remoteProxyName(rr.Name)+"-admin", rr.Namespace).
		WithLabels(rrIdentityLabels(rr)).
		WithOwnerReferences(ownerRefForRR(rr)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(rrIdentityLabels(rr)).
			WithPorts(corev1ac.ServicePort().
				WithName("admin").
				WithPort(9901).
				WithTargetPort(intstr.FromInt(9902)).
				WithProtocol(corev1.ProtocolTCP),
			),
		)
}

func renderRemoteDeployment(rr *routeprismv1alpha1.RemoteRoute, conf string) *appsv1ac.DeploymentApplyConfiguration {
	labels := rrIdentityLabels(rr)
	replicas := int32(1)
	return appsv1ac.Deployment(remoteProxyName(rr.Name), rr.Namespace).
		WithLabels(labels).
		WithOwnerReferences(ownerRefForRR(rr)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(replicas).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(labels)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				WithAnnotations(map[string]string{
					"route-prism.egoavara.net/config-checksum": configChecksum(conf),
				}).
				WithSpec(corev1ac.PodSpec().
					WithContainers(corev1ac.Container().
						WithName("envoy").
						WithImage("envoyproxy/envoy:v1.31-latest").
						WithArgs("-c", "/etc/envoy/envoy.yaml", "--log-level", "warn").
						WithPorts(
							corev1ac.ContainerPort().WithName("http").WithContainerPort(8080),
							corev1ac.ContainerPort().WithName("admin").WithContainerPort(9902),
						).
						WithResources(corev1ac.ResourceRequirements().
							WithRequests(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("20m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							}).
							WithLimits(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							}),
						).
						WithVolumeMounts(corev1ac.VolumeMount().
							WithName("conf").
							WithMountPath("/etc/envoy"),
						).
						WithReadinessProbe(corev1ac.Probe().
							WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromInt(8080))).
							WithPeriodSeconds(5),
						).
						WithLivenessProbe(corev1ac.Probe().
							WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromInt(8080))).
							WithPeriodSeconds(10).
							WithFailureThreshold(3),
						),
					).
					WithVolumes(corev1ac.Volume().
						WithName("conf").
						WithConfigMap(corev1ac.ConfigMapVolumeSource().
							WithName(remoteProxyName(rr.Name)),
						),
					),
				),
			),
		)
}
