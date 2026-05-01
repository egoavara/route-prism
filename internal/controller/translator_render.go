/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
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
//  1. Cookie parsing: the translator extracts the entry whose key equals
//     RoutingKey from the multi-key cookie value
//     "k1:v1|k2:v2|...". Other entries are ignored by *this* translator
//     (they're meant for sibling translators on other tiers).
//
//  2. Baggage emission: every non-`.` entry observed in the cookie is
//     replayed verbatim into the W3C Baggage header so downstream tiers
//     can use either the cookie or the baggage as the source of truth.
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

	// Widget injection (optional). Widget.Enable gates everything; the
	// other fields are zero-safe when disabled.
	Widget WidgetTmplData
	// WidgetConfigJSON is a single-line JSON pre-marshaled at render time
	// so the Lua filter doesn't have to re-encode it per request. Single
	// quotes are HTML-escaped (`'` → `&#39;`) so it's safe to embed
	// verbatim inside a single-quoted HTML attribute.
	WidgetConfigJSON string
	// OperatorAPIAddr is the in-cluster host:port for the operator API
	// Service. Used as the upstream for the widget asset proxy and the
	// /api/v1 reverse-proxy mount.
	OperatorAPIAddr string
}

// WidgetTmplData carries the per-translator widget knobs into the nginx
// template. Header / cookie matchers are pre-compiled into Lua-friendly
// shapes so the runtime path is a straight lookup.
type WidgetTmplData struct {
	Enable     bool
	PathPrefix string

	// IPCIDRs feeds nginx `geo` directly.
	IPCIDRs           []string
	TrustForwardedFor bool

	// Headers / Cookies / Methods / PathPrefixes / Hostnames are evaluated
	// per-request in Lua. Empty list = the corresponding gate is disabled.
	Headers      []TmplHeaderMatch
	Cookies      []TmplCookieMatch
	Methods      []string
	PathPrefixes []string
	// Hostnames precompiled to Lua patterns (^…$). `*.example.com` →
	// `^[^.]+%.example%.com$`.
	HostnamePatterns []string
}

// TmplHeaderMatch / TmplCookieMatch carry a single matcher in a form the
// Lua filter understands. Exactly one of (Value, Regex, Present=true) is
// set per row. Regex is a PCRE pattern (`ngx.re.match` compatible).
type TmplHeaderMatch struct {
	Name    string
	Value   string
	Regex   string
	Present bool
}
type TmplCookieMatch = TmplHeaderMatch

// defaultWidgetPathPrefix mirrors the CRD default. Used as a defensive
// fallback when an older object lacks the field after schema upgrade.
const defaultWidgetPathPrefix = ".route-prism"

// renderNginxConf produces the translator nginx config. The translator
// always forwards directly to a Pod IP (no mesh second hop) so it never
// re-enters the target Service's HTTPRoute — that's why we no longer
// stamp a "checked" loop-prevention marker. When no ContextRoute attaches
// to the target, variants is empty and every request goes to defaultPodIP.
//
// operatorAPIAddr is the in-cluster host:port of the operator API Service
// — the upstream that serves the widget bundle and the proxied /api/v1.
// It can be empty when widget injection is disabled.
func renderNginxConf(et *routeprismv1alpha1.EdgeTransformation, routingKey, dnsClusterIP, defaultPodIP string, defaultPort int32, variants []VariantBackend, operatorAPIAddr string) (string, error) {
	if dnsClusterIP == "" {
		dnsClusterIP = fallbackDNSClusterIP
	}
	// Sort variants for deterministic output.
	sorted := make([]VariantBackend, len(variants))
	copy(sorted, variants)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CookieValue < sorted[j].CookieValue })

	widget, widgetCfgJSON, err := buildWidgetTmplData(et, routingKey)
	if err != nil {
		return "", err
	}

	data := nginxConfData{
		RoutingKey:       routingKey,
		SourceCookie:     et.Spec.SourceCookie,
		Namespace:        et.Namespace,
		DNSClusterIP:     dnsClusterIP,
		DefaultPodIP:     defaultPodIP,
		DefaultPort:      defaultPort,
		Variants:         sorted,
		Widget:           widget,
		WidgetConfigJSON: widgetCfgJSON,
		OperatorAPIAddr:  operatorAPIAddr,
	}
	var buf bytes.Buffer
	if err := nginxSmartTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nginx smart: %w", err)
	}
	return buf.String(), nil
}

// buildWidgetTmplData translates the WidgetInjection CRD spec into the
// flat shape the nginx template expects, plus a pre-marshaled JSON config
// blob the translator stamps onto the injected <script> tag.
func buildWidgetTmplData(et *routeprismv1alpha1.EdgeTransformation, routingKey string) (WidgetTmplData, string, error) {
	if et.Spec.WidgetInjection == nil || !et.Spec.WidgetInjection.Enable {
		return WidgetTmplData{Enable: false}, "", nil
	}
	wi := et.Spec.WidgetInjection
	prefix := wi.HTTP.PathPrefix
	if prefix == "" {
		prefix = defaultWidgetPathPrefix
	}

	w := WidgetTmplData{
		Enable:            true,
		PathPrefix:        prefix,
		IPCIDRs:           append([]string(nil), wi.Target.Match.IPs...),
		TrustForwardedFor: wi.Target.Match.TrustForwardedFor,
		Methods:           append([]string(nil), wi.Target.Match.Methods...),
		PathPrefixes:      append([]string(nil), wi.Target.Match.PathPrefixes...),
	}
	for _, h := range wi.Target.Match.Headers {
		w.Headers = append(w.Headers, TmplHeaderMatch{
			Name: h.Name, Value: h.Value, Regex: h.Regex, Present: h.Present,
		})
	}
	for _, c := range wi.Target.Match.Cookies {
		w.Cookies = append(w.Cookies, TmplCookieMatch{
			Name: c.Name, Value: c.Value, Regex: c.Regex, Present: c.Present,
		})
	}
	for _, host := range wi.Target.Match.Hostnames {
		w.HostnamePatterns = append(w.HostnamePatterns, hostnameGlobToLuaPattern(host))
	}

	// Build the per-page widget config JSON. The widget JS reads this
	// from data-route-prism-config on its <script> tag.
	cfg := map[string]any{
		"target":       et.Spec.Target.Service.Name,
		"namespace":    et.Namespace,
		"routingKey":   routingKey,
		"sourceCookie": et.Spec.SourceCookie,
		"pathPrefix":   prefix,
		"style": map[string]any{
			"mode": stringOrDefault(string(wi.Style.Mode), "float"),
			"margin": map[string]string{
				"top":    wi.Style.Margin.Top,
				"right":  wi.Style.Margin.Right,
				"bottom": wi.Style.Margin.Bottom,
				"left":   wi.Style.Margin.Left,
			},
			"hotkey": map[string]string{
				"open": wi.Style.Hotkey.Open,
			},
		},
		"js": map[string]any{
			"fetch":          map[string]any{"enable": wi.JS.Fetch.Enable},
			"xmlhttprequest": map[string]any{"enable": wi.JS.XMLHttpRequest.Enable},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return WidgetTmplData{}, "", fmt.Errorf("marshal widget config: %w", err)
	}
	// We embed inside a single-quoted attribute, so escape '. & < > are
	// fine because JSON doesn't use them in keys/string-syntax outside of
	// values, and json.Marshal already escapes <, >, & to \u00XX when we
	// set HTMLEscape in encoder; here we use Marshal directly which does
	// NOT escape <, > but that's safe inside an attribute value.
	cfgStr := strings.ReplaceAll(string(raw), "'", "&#39;")
	return w, cfgStr, nil
}

// hostnameGlobToLuaPattern converts a hostname glob (`*.example.com` or
// literal `api.example.com`) into a Lua pattern anchored at both ends.
// Supports a single leading `*.` wildcard for one subdomain label.
func hostnameGlobToLuaPattern(glob string) string {
	const wildcardPrefix = "*."
	if strings.HasPrefix(glob, wildcardPrefix) {
		rest := glob[len(wildcardPrefix):]
		return `^[^.]+%.` + luaPatternEscape(rest) + `$`
	}
	return `^` + luaPatternEscape(glob) + `$`
}

// luaPatternEscape escapes characters that have special meaning in Lua
// pattern syntax (different from PCRE; only `^$().%[]*+-?` are magic).
var luaSpecialRe = regexp.MustCompile(`([%^$()%%.\[\]*+\-?])`)

func luaPatternEscape(s string) string {
	return luaSpecialRe.ReplaceAllString(s, `%$1`)
}

func stringOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func translatorLabels(et *routeprismv1alpha1.EdgeTransformation) map[string]string {
	return map[string]string{
		LabelManagedBy:               ManagedByValue,
		LabelKind:                    KindEdgeTransformation,
		LabelOwner:                   et.Name,
		"app.kubernetes.io/name":     "route-prism-translator",
		"app.kubernetes.io/instance": et.Name,
	}
}

func ownerRefForET(et *routeprismv1alpha1.EdgeTransformation) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         routeprismv1alpha1.GroupVersion.String(),
		Kind:               "EdgeTransformation",
		Name:               et.Name,
		UID:                et.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(true),
	}
}

func renderTranslatorConfigMap(et *routeprismv1alpha1.EdgeTransformation, conf string) *corev1.ConfigMap {
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

func renderTranslatorService(et *routeprismv1alpha1.EdgeTransformation) *corev1.Service {
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

func renderTranslatorDeployment(et *routeprismv1alpha1.EdgeTransformation, conf string) *appsv1.Deployment {
	labels := translatorLabels(et)
	replicas := int32(1)
	podLabels := make(map[string]string, len(labels))
	maps.Copy(podLabels, labels)
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
