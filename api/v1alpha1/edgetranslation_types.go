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

// EdgeMode selects the translator implementation.
// +kubebuilder:validation:Enum=router;envoy-lua;envoy-wasm
type EdgeMode string

const (
	EdgeModeRouter    EdgeMode = "router"
	EdgeModeEnvoyLua  EdgeMode = "envoy-lua"
	EdgeModeEnvoyWasm EdgeMode = "envoy-wasm"
)

// (CheckedKey removed: smart-mode translators now forward directly to a
// variant Pod IP, so traffic never re-enters the target Service after
// translation and no loop-prevention marker is required.)

// EdgeTranslationSpec defines the desired state of EdgeTranslation.
//
// The controller emits a single HTTPRoute attached to the target Service
// with two rules: rule A (already translated, baggage carries the checked
// marker) routes to the target itself; rule B (catch-all) routes to the
// nginx translator. The translator extracts the source cookie, stamps a
// baggage member, adds the checked marker, and proxies back to the target.
type EdgeTranslationSpec struct {
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
	// and translated.
	// +required
	Target TargetReference `json:"target"`
}

// EdgeTranslationStatus defines the observed state of EdgeTranslation.
type EdgeTranslationStatus struct {
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
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.translatorReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeTranslation is the Schema for the edgetranslations API.
type EdgeTranslation struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec EdgeTranslationSpec `json:"spec"`

	// +optional
	Status EdgeTranslationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

type EdgeTranslationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EdgeTranslation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EdgeTranslation{}, &EdgeTranslationList{})
}
