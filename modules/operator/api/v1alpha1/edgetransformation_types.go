/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EdgeMode selects the translator implementation.
// +kubebuilder:validation:Enum=router;envoy-lua;envoy-wasm
type EdgeMode string

const (
	EdgeModeRouter    EdgeMode = "router"
	EdgeModeEnvoyLua  EdgeMode = "envoy-lua"
	EdgeModeEnvoyWasm EdgeMode = "envoy-wasm"
)

// EdgeTransformationSpec defines the desired state of EdgeTransformation.
//
// The controller emits a single HTTPRoute attached to the target Service
// that sends every request through the translator. The translator performs
// two kinds of edge transformation:
//
//   - Always: cookie → baggage. Extracts SourceCookie, parses the multi-tier
//     value "<routingKey>:<variant>|...", picks the matching entry, and
//     stamps the W3C Baggage header on the upstream request.
//   - Optional (WidgetInjection.Enable): inject a floating in-page widget
//     into HTML responses so users can toggle their own routing context
//     directly from the browser.
type EdgeTransformationSpec struct {
	// Mode selects the translator implementation. Prototype only supports
	// "router" (a small nginx Deployment provisioned by the controller).
	// +required
	Mode EdgeMode `json:"mode"`

	// SourceCookie is the cookie key whose value will be lifted into the
	// baggage member "x-route-prism=<value>".
	// +kubebuilder:validation:MinLength=1
	// +required
	SourceCookie string `json:"sourceCookie"`

	// Target is the Service whose inbound traffic should be intercepted
	// and transformed.
	// +required
	Target TargetReference `json:"target"`

	// WidgetInjection, when present and Enable=true, injects an in-page
	// widget into HTML responses passing through this translator. The
	// widget calls the operator API (proxied through the translator under
	// HTTP.PathPrefix) to let users switch routing context from the
	// browser without going to a separate dashboard.
	// +optional
	WidgetInjection *WidgetInjectionSpec `json:"widgetInjection,omitempty"`
}

// WidgetInjectionSpec configures the in-page widget. Disabled by default.
type WidgetInjectionSpec struct {
	// Enable turns the widget on. When false (default) the translator
	// behaves exactly as before — pure cookie→baggage with no body
	// rewriting.
	// +kubebuilder:default=false
	// +optional
	Enable bool `json:"enable"`

	// HTTP carries URL-routing knobs for the widget asset and proxied API.
	// +optional
	HTTP WidgetHTTPSpec `json:"http,omitempty"`

	// Target controls *which* responses get the widget injected. Leaving
	// every match field empty means "inject for all clients on all HTML
	// responses".
	// +optional
	Target WidgetTargetSpec `json:"target,omitempty"`

	// Style controls the widget's UI presentation (mounting mode, margins,
	// hotkey). The values are passed through to the widget JS via a JSON
	// blob on the injected <script> tag.
	// +optional
	Style WidgetStyleSpec `json:"style,omitempty"`

	// JS configures client-side behavior the widget bootstraps in the
	// browser. Notably, when the page calls cross-origin APIs (web on one
	// domain, api on another) browser cookies are dropped on the request
	// and cookie-based variant routing breaks. Enabling fetch/XHR override
	// makes the widget attach the active routing context as a request
	// header (default "X-Route-Prism") on every outbound call so the
	// translator can read it on the API side regardless of origin.
	// +optional
	JS WidgetJSSpec `json:"js,omitempty"`
}

// WidgetJSSpec controls JavaScript-level behavior installed by the widget.
// When any override is enabled the widget monkey-patches the global
// fetch / XMLHttpRequest at boot so every outbound HTTP request carries
// the active routing context as the W3C Baggage header. Cookies are
// dropped on cross-origin requests, but Baggage rides through unchanged
// — and the translator already reads Baggage as the inter-tier wire
// format, so no additional schema is needed on the receiving side.
type WidgetJSSpec struct {
	// Fetch toggles the window.fetch override.
	// +optional
	Fetch WidgetJSOverride `json:"fetch,omitempty"`

	// XMLHttpRequest toggles the XHR.send override (legacy call sites).
	// +optional
	XMLHttpRequest WidgetJSOverride `json:"xmlhttprequest,omitempty"`
}

// WidgetJSOverride is a small shape so each override slot can grow its
// own knobs (allow/deny lists, etc.) without breaking the JSON shape.
type WidgetJSOverride struct {
	// Enable turns the override on.
	// +kubebuilder:default=false
	// +optional
	Enable bool `json:"enable,omitempty"`
}

// WidgetHTTPSpec reserves URL paths under PathPrefix on the translator's
// HTTP surface for the widget bundle and proxied operator API. The default
// prefix ".route-prism" leads with a dot to make collisions with real app
// routes vanishingly unlikely.
type WidgetHTTPSpec struct {
	// PathPrefix without leading or trailing slash. The translator
	// reserves "/<pathPrefix>/widget/" for the bundle and
	// "/<pathPrefix>/api/" for proxied operator API calls.
	// +kubebuilder:default=".route-prism"
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`
	// +optional
	PathPrefix string `json:"pathPrefix,omitempty"`
}

// WidgetTargetSpec narrows widget injection to specific clients/requests.
type WidgetTargetSpec struct {
	// Match is an AND of all set conditions. When all fields are unset
	// the widget is injected for every HTML response.
	// +optional
	Match WidgetMatchSpec `json:"match,omitempty"`
}

// WidgetMatchSpec is a Gateway-API-flavored match: each named condition is
// an AND between conditions, OR within a list (except Headers/Cookies, which
// are AND because each entry names a distinct cookie/header).
type WidgetMatchSpec struct {
	// IPs is a list of CIDRs. The client IP must fall in at least one to
	// pass (OR). Empty list disables the IP gate.
	// +optional
	IPs []string `json:"ips,omitempty"`

	// TrustForwardedFor: when true, the leftmost IP from
	// X-Forwarded-For is used as the client IP for the IPs match.
	// Required when the translator runs behind a mesh sidecar or proxy.
	// +kubebuilder:default=false
	// +optional
	TrustForwardedFor bool `json:"trustForwardedFor,omitempty"`

	// Headers — each entry must match (AND). Use Value (exact),
	// Regex (PCRE), or Present (existence only).
	// +optional
	Headers []HeaderMatch `json:"headers,omitempty"`

	// Cookies — each entry must match (AND).
	// +optional
	Cookies []CookieMatch `json:"cookies,omitempty"`

	// PathPrefixes — request path must start with at least one (OR).
	// Empty list = all paths.
	// +optional
	PathPrefixes []string `json:"pathPrefixes,omitempty"`

	// Methods — request method must be in the list (OR). Defaults to
	// ["GET"] because widget injection only makes sense for HTML page
	// loads, which are GET in practice.
	// +kubebuilder:default={GET}
	// +optional
	Methods []string `json:"methods,omitempty"`

	// Hostnames — request Host header must match at least one entry (OR).
	// Glob form supported: leading "*." matches any single subdomain
	// label. Empty list = all hostnames.
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`
}

// HeaderMatch matches one HTTP request header. Exactly one of Value, Regex,
// Present must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.value)?1:0)+(has(self.regex)?1:0)+(self.present?1:0) == 1",message="exactly one of value, regex, present must be set"
type HeaderMatch struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
	// +optional
	Value string `json:"value,omitempty"`
	// +optional
	Regex string `json:"regex,omitempty"`
	// +optional
	Present bool `json:"present,omitempty"`
}

// CookieMatch matches one cookie on the request. Exactly one of Value,
// Regex, Present must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.value)?1:0)+(has(self.regex)?1:0)+(self.present?1:0) == 1",message="exactly one of value, regex, present must be set"
type CookieMatch struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
	// +optional
	Value string `json:"value,omitempty"`
	// +optional
	Regex string `json:"regex,omitempty"`
	// +optional
	Present bool `json:"present,omitempty"`
}

// WidgetStyleMode picks the widget's mounting layout.
// +kubebuilder:validation:Enum=float;sidebar
type WidgetStyleMode string

const (
	WidgetStyleFloat   WidgetStyleMode = "float"
	WidgetStyleSidebar WidgetStyleMode = "sidebar"
)

// WidgetStyleSpec carries presentation knobs forwarded to the widget JS.
type WidgetStyleSpec struct {
	// Mode picks the widget layout. "float" pins a circular toggle to a
	// viewport corner; "sidebar" pins a vertical edge tab.
	// +kubebuilder:default=float
	// +optional
	Mode WidgetStyleMode `json:"mode,omitempty"`

	// Margin nudges the widget away from the viewport edges. Values are
	// passed through verbatim as CSS lengths (e.g. "16px", "1rem").
	// +optional
	Margin WidgetMarginSpec `json:"margin,omitempty"`

	// Hotkey lets users open the widget from the keyboard.
	// +optional
	Hotkey WidgetHotkeySpec `json:"hotkey,omitempty"`
}

type WidgetMarginSpec struct {
	// +optional
	Left string `json:"left,omitempty"`
	// +optional
	Right string `json:"right,omitempty"`
	// +optional
	Top string `json:"top,omitempty"`
	// +optional
	Bottom string `json:"bottom,omitempty"`
}

type WidgetHotkeySpec struct {
	// Open is a chord like "ctrl+\\" or "ctrl+shift+k". Empty disables
	// the hotkey. Modifiers: ctrl, shift, alt, meta. The final segment
	// is the key (case-insensitive).
	// +optional
	Open string `json:"open,omitempty"`
}

// EdgeTransformationStatus defines the observed state of EdgeTransformation.
type EdgeTransformationStatus struct {
	// TranslatorReady is true once the provisioned Deployment reports >=1
	// available replica.
	// +optional
	TranslatorReady bool `json:"translatorReady"`

	// TargetService is the resolved target Service name (echo of spec).
	// +optional
	TargetService string `json:"targetService,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=et
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.service.name`
// +kubebuilder:printcolumn:name="Widget",type=boolean,JSONPath=`.spec.widgetInjection.enable`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.translatorReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeTransformation is the Schema for the edgetransformations API.
type EdgeTransformation struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec EdgeTransformationSpec `json:"spec"`

	// +optional
	Status EdgeTransformationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

type EdgeTransformationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EdgeTransformation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EdgeTransformation{}, &EdgeTransformationList{})
}
