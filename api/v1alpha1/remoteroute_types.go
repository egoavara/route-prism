/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RRContextRouteRef points at a sibling ContextRoute in the same namespace.
// RemoteRoute is hard-bound to its ContextRoute: the controller installs an
// ownerReference pointing at the CR so deleting the CR garbage-collects
// every RemoteRoute attached to it (and the proxy workload it owns).
type RRContextRouteRef struct {
	// Name of the ContextRoute in the same namespace as this RemoteRoute.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// RemoteRouteSpec defines the desired state of RemoteRoute.
//
// A RemoteRoute attaches to a ContextRoute and provisions a small Envoy
// reverse-proxy Deployment plus a ClusterIP Service. The Service inherits
// the parent ContextRoute's variant matchLabels at reconcile time so once
// the proxy comes up it shows up as a regular variant on the CR's
// HTTPRoute — Baggage value "<routingKey>=<remoteroute-name>" then routes
// traffic through Envoy out to the operator's developer PC.
//
// The proxy is intentionally lenient: the upstream PC may be unreachable
// for long stretches without taking the proxy itself down. Requests during
// downtime surface as 502/504 from Envoy instead of the pod failing its
// own liveness check.
type RemoteRouteSpec struct {
	// ContextRouteRef binds this RemoteRoute to a ContextRoute. The CR's
	// variant selector (matchLabels only) is read at reconcile time and
	// copied onto the proxy Service so the variant is auto-discovered.
	// +required
	ContextRouteRef RRContextRouteRef `json:"contextRouteRef"`

	// Upstreams are the candidate destinations for traffic, in priority
	// order — index 0 is highest priority. Envoy routes traffic to the
	// highest-priority entry whose health check passes; if all endpoints
	// in priority 0 are unhealthy, traffic spills to priority 1, and so
	// on. Within a single multi-URL group, endpoints are load-balanced
	// according to the group's mode.
	//
	// Constraints:
	//   - Every URL across all entries must share the same scheme
	//     (all http or all https) — mixed-scheme RemoteRoutes are
	//     rejected (Ready=False/MixedScheme).
	//   - Every multi-URL group must declare the same mode — Envoy
	//     applies one load-balancing policy cluster-wide.
	// +required
	// +kubebuilder:validation:MinItems=1
	Upstreams []RemoteUpstream `json:"upstreams"`

	// TLS configures the upstream TLS transport. Has effect only when
	// the upstream URLs use the https scheme.
	// +optional
	TLS *RemoteRouteTLS `json:"tls,omitempty"`

	// HealthCheck overrides the default proxy active health check. When
	// omitted, a TCP connect probe is used (interval=5s, timeout=1s).
	// +optional
	HealthCheck *RemoteHealthCheck `json:"healthCheck,omitempty"`
}

// RemoteUpstream is one entry in the priority-ordered upstream list.
// Exactly one of URL, Group must be set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.url)?1:0)+(has(self.group)?1:0) == 1",message="exactly one of url, group must be set"
type RemoteUpstream struct {
	// Name identifies this entry in status reports. Must be unique
	// within the RemoteRoute.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// URL is a single upstream URL. Mutually exclusive with Group.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+(/.*)?$`
	// +optional
	URL string `json:"url,omitempty"`

	// Group is a multi-URL upstream that load-balances within a single
	// priority slot. Mutually exclusive with URL.
	// +optional
	Group *RemoteUpstreamGroup `json:"group,omitempty"`
}

// RemoteLBMode picks the load-balancing policy applied within a group AND
// (because Envoy clusters have one cluster-wide policy) cluster-wide. All
// groups in one RemoteRoute must declare the same mode.
//
// +kubebuilder:validation:Enum=RoundRobin;Random
type RemoteLBMode string

const (
	RemoteLBRoundRobin RemoteLBMode = "RoundRobin"
	RemoteLBRandom     RemoteLBMode = "Random"
)

// RemoteUpstreamGroup is a set of URLs sharing one priority slot.
type RemoteUpstreamGroup struct {
	// Mode is the load-balancing policy used to pick among the URLs.
	// +kubebuilder:default=RoundRobin
	// +optional
	Mode RemoteLBMode `json:"mode,omitempty"`

	// URLs is the list of upstream URLs in this group.
	// +required
	// +kubebuilder:validation:MinItems=1
	URLs []string `json:"urls"`
}

// RemoteRouteTLS configures upstream TLS knobs.
type RemoteRouteTLS struct {
	// InsecureSkipVerify, when true, disables certificate chain and
	// hostname verification on the upstream TLS connection. Useful for
	// dev PCs running self-signed certificates. Defaults to false.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// RemoteHealthCheckType selects the active health-check protocol Envoy
// uses to probe upstreams.
//
// +kubebuilder:validation:Enum=TCP;HTTP
type RemoteHealthCheckType string

const (
	RemoteHealthCheckTCP  RemoteHealthCheckType = "TCP"
	RemoteHealthCheckHTTP RemoteHealthCheckType = "HTTP"
)

// RemoteHealthCheck overrides the default proxy active health check.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'HTTP' || has(self.http)",message="http must be set when type is HTTP"
type RemoteHealthCheck struct {
	// Type picks the probe protocol. Defaults to TCP when this whole
	// block is omitted; when the block is present, Type is required.
	// +kubebuilder:default=TCP
	// +required
	Type RemoteHealthCheckType `json:"type"`

	// HTTP carries the HTTP probe knobs; required when Type=HTTP.
	// +optional
	HTTP *RemoteHTTPHealthCheck `json:"http,omitempty"`
}

// RemoteHTTPHealthCheck describes an HTTP-level probe.
type RemoteHTTPHealthCheck struct {
	// Path is the request path Envoy GETs. Must start with `/`.
	// +kubebuilder:validation:Pattern=`^/.*`
	// +required
	Path string `json:"path"`

	// Host overrides the Host header on the probe request. When empty,
	// Envoy uses the upstream cluster name.
	// +optional
	Host string `json:"host,omitempty"`
}

// RemoteUpstreamStatus is the observed state of one upstream entry.
type RemoteUpstreamStatus struct {
	// Name echoes spec.upstreams[i].name.
	Name string `json:"name"`

	// URLs lists the resolved URL(s). For URL entries this has one
	// item; for Group entries it mirrors group.urls.
	URLs []string `json:"urls"`

	// Healthy is true when at least one endpoint in this priority slot
	// passed Envoy's last active health check.
	// +optional
	Healthy bool `json:"healthy"`

	// HealthyEndpoints is the count of currently-healthy endpoints in
	// this slot (max = len(URLs)).
	// +optional
	HealthyEndpoints int32 `json:"healthyEndpoints"`

	// LastProbeTime is the last time the controller polled the proxy's
	// admin endpoint to refresh this entry.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// Message carries additional human-readable detail (e.g.
	// "connection refused", "tls handshake failed").
	// +optional
	Message string `json:"message,omitempty"`
}

// RemoteRouteStatus defines the observed state of RemoteRoute.
type RemoteRouteStatus struct {
	// ProxyReady is true once the provisioned Envoy Deployment reports
	// >= 1 available replica. Independent of upstream reachability.
	// +optional
	ProxyReady bool `json:"proxyReady"`

	// VariantName is the cookie/baggage value that routes traffic
	// through this RemoteRoute. Always equal to metadata.name; surfaced
	// here so external consumers (widget, dashboard) have a single
	// status field to read.
	// +optional
	VariantName string `json:"variantName,omitempty"`

	// ActiveUpstream is the name of the highest-priority upstream entry
	// currently considered healthy by Envoy. Empty when every upstream
	// is unhealthy.
	// +optional
	ActiveUpstream string `json:"activeUpstream,omitempty"`

	// Upstreams reports per-entry health. Index parallels spec.upstreams.
	// +optional
	Upstreams []RemoteUpstreamStatus `json:"upstreams,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rr
// +kubebuilder:printcolumn:name="ContextRoute",type=string,JSONPath=`.spec.contextRouteRef.name`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeUpstream`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=='UpstreamReachable')].status`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.proxyReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemoteRoute is the Schema for the remoteroutes API.
type RemoteRoute struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec RemoteRouteSpec `json:"spec"`

	// +optional
	Status RemoteRouteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RemoteRouteList contains a list of RemoteRoute.
type RemoteRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RemoteRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RemoteRoute{}, &RemoteRouteList{})
}
