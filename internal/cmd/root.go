/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package cmd assembles the route-prism CLI on top of cobra/viper. The
// root command owns the shared configuration tree (resolved via flags +
// env + optional YAML) and dispatches to subcommands such as `run`.
package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/egoavara/route-prism/internal/config"
)

const (
	// envPrefix becomes ROUTE_PRISM_… when used with viper's auto-env.
	// Dots in keys map to underscores: metrics.bind-address →
	// ROUTE_PRISM_METRICS_BIND_ADDRESS.
	envPrefix = "ROUTE_PRISM"
)

// Execute is the entrypoint called from main(). It returns the exit error
// without printing it itself — cobra already prints command-level errors.
func Execute() error {
	root := newRoot()
	return root.Execute()
}

func newRoot() *cobra.Command {
	v := viper.New()
	cfg := &config.Config{}

	root := &cobra.Command{
		Use:           "route-prism",
		Short:         "Context-aware GAMMA routing controller",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	var cfgFile string
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "Path to a YAML config file (optional)")

	bindFlags(root, v)

	cobra.OnInitialize(func() {
		initViper(v, cfgFile)
		if err := v.Unmarshal(cfg); err != nil {
			cobra.CheckErr(fmt.Errorf("unmarshal config: %w", err))
		}
	})

	root.AddCommand(newRunCmd(cfg, v))
	return root
}

// bindFlags registers every persistent flag and binds it to a viper key.
// Keeping flag names and viper keys aligned (via mapstructure tags in
// internal/config) is what makes `viper.Unmarshal` produce a populated
// Config.
func bindFlags(cmd *cobra.Command, v *viper.Viper) {
	pf := cmd.PersistentFlags()

	pf.String("metrics.bind-address", "0", "controller-runtime metrics bind address (\"0\" disables, \":8443\" HTTPS, \":8080\" HTTP)")
	pf.Bool("metrics.secure", true, "Serve controller-runtime metrics over HTTPS")
	pf.String("metrics.cert-path", "", "Directory containing the metrics server certificate")
	pf.String("metrics.cert-name", "tls.crt", "Metrics server certificate file name")
	pf.String("metrics.cert-key", "tls.key", "Metrics server key file name")

	pf.String("webhook.cert-path", "", "Directory containing the webhook certificate")
	pf.String("webhook.cert-name", "tls.crt", "Webhook certificate file name")
	pf.String("webhook.cert-key", "tls.key", "Webhook key file name")

	pf.String("health.probe-bind-address", ":8081", "Liveness/readiness probe bind address")

	pf.String("api.bind-address", ":8082", "Read-only routing API bind address (empty disables)")
	pf.String("api.advertised-address", "route-prism-controller-manager-api.route-prism-system.svc.cluster.local:8082",
		"In-cluster host:port that translators use to reach the operator API (widget assets + /api/v1 proxy)")

	pf.Bool("leader.enabled", false, "Enable leader election for the controller manager")
	pf.Bool("enable-http2", false, "Enable HTTP/2 for the metrics and webhook servers")

	pf.String("otel.service-name", "route-prism", "OTel service.name resource attribute")
	pf.String("otel.endpoint", "", "OTLP/gRPC collector endpoint (host:port). Empty falls back to OTEL_EXPORTER_OTLP_ENDPOINT")
	pf.Bool("otel.insecure", false, "Use plaintext gRPC for OTLP exporters")

	pf.Bool("otel.metrics.enabled", false, "Export metrics via OTLP")
	pf.Bool("otel.traces.enabled", false, "Export traces via OTLP")
	pf.Bool("otel.logs.enabled", false, "Export logs via OTLP (also forks zap output into the OTel log pipeline)")

	pf.Bool("otel.prometheus.enabled", false, "Expose OTel metrics via a Prometheus pull exporter")
	pf.String("otel.prometheus.bind-address", "", "Dedicated Prometheus listener (empty mounts the handler on the API server)")
	pf.String("otel.prometheus.path", "/metrics", "HTTP path for the Prometheus exporter")

	pf.VisitAll(func(f *pflag.Flag) {
		_ = v.BindPFlag(f.Name, f)
	})
}

func initViper(v *viper.Viper, cfgFile string) {
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
		if err := v.ReadInConfig(); err != nil {
			cobra.CheckErr(fmt.Errorf("read config file %q: %w", cfgFile, err))
		}
	}
}
