/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package config holds the typed runtime configuration for route-prism.
// It is populated by the cobra/viper layer in internal/cmd and consumed
// by the manager bootstrap, the API server, and the observability addons.
package config

// Config is the root configuration tree.
type Config struct {
	Metrics Metrics `mapstructure:"metrics"`
	Webhook Webhook `mapstructure:"webhook"`
	Health  Health  `mapstructure:"health"`
	API     API     `mapstructure:"api"`
	Leader  Leader  `mapstructure:"leader"`
	HTTP2   bool    `mapstructure:"enable-http2"`
	Otel    Otel    `mapstructure:"otel"`
}

type Metrics struct {
	BindAddress string `mapstructure:"bind-address"`
	Secure      bool   `mapstructure:"secure"`
	CertPath    string `mapstructure:"cert-path"`
	CertName    string `mapstructure:"cert-name"`
	CertKey     string `mapstructure:"cert-key"`
}

type Webhook struct {
	CertPath string `mapstructure:"cert-path"`
	CertName string `mapstructure:"cert-name"`
	CertKey  string `mapstructure:"cert-key"`
}

type Health struct {
	ProbeBindAddress string `mapstructure:"probe-bind-address"`
}

type API struct {
	BindAddress string `mapstructure:"bind-address"`
	// AdvertisedAddress is the in-cluster address (host:port) translators
	// use to reach the operator API for widget assets and the proxied
	// read API. Defaults to the kustomize Service DNS name.
	AdvertisedAddress string `mapstructure:"advertised-address"`
}

type Leader struct {
	Enabled bool `mapstructure:"enabled"`
}

// Otel groups every OpenTelemetry-driven signal pipeline. Each signal
// (Metrics / Traces / Logs / Prometheus) has its own Enabled toggle so an
// operator can opt in selectively — e.g. ship traces over OTLP while
// keeping metrics on in-cluster Prometheus scraping.
//
// Shared transport options (Endpoint / Insecure / ServiceName) live at
// this level because all OTLP-based signals reuse the same collector.
type Otel struct {
	ServiceName string `mapstructure:"service-name"`
	// Endpoint is the OTLP/gRPC collector endpoint, e.g. "otel-collector:4317".
	// Empty falls back to the standard OTEL_EXPORTER_OTLP_ENDPOINT env var.
	Endpoint string `mapstructure:"endpoint"`
	Insecure bool   `mapstructure:"insecure"`

	Metrics    OtelSignal `mapstructure:"metrics"`
	Traces     OtelSignal `mapstructure:"traces"`
	Logs       OtelSignal `mapstructure:"logs"`
	Prometheus Prometheus `mapstructure:"prometheus"`
}

// OtelSignal is the minimal per-signal config. Kept as a struct (not a
// bare bool) so signal-specific options can be added later without
// breaking the flag/env layout.
type OtelSignal struct {
	Enabled bool `mapstructure:"enabled"`
}

// Prometheus configures the Prometheus pull exporter. When Enabled, it is
// attached as an additional reader on the OTel meter provider — the same
// instruments end up scrapeable at /metrics and pushable via OTLP.
type Prometheus struct {
	Enabled bool `mapstructure:"enabled"`
	// BindAddress hosts a dedicated /metrics listener (e.g. ":9464"). Empty
	// means the handler is returned to the caller for mounting elsewhere
	// (typical: hand it to the API server's chi router).
	BindAddress string `mapstructure:"bind-address"`
	Path        string `mapstructure:"path"`
}
