/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"context"
	"crypto/tls"
	stdflag "flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	zaplog "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
	"github.com/egoavara/route-prism/internal/apiserver"
	"github.com/egoavara/route-prism/internal/config"
	"github.com/egoavara/route-prism/internal/controller"
	"github.com/egoavara/route-prism/internal/observability"
	"github.com/egoavara/route-prism/internal/preflight"
	// +kubebuilder:scaffold:imports
)

func newRunCmd(cfg *config.Config, _ *viper.Viper) *cobra.Command {
	zapOpts := zap.Options{Development: true}
	zapFS := stdflag.NewFlagSet("zap", stdflag.ContinueOnError)
	zapOpts.BindFlags(zapFS)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the controller manager and the routing API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runManager(cmd.Context(), cfg, &zapOpts)
		},
	}

	// Bridge controller-runtime's zap flags (defined on a stdlib FlagSet)
	// into cobra's pflag so users see them via --help and env binding.
	cmd.Flags().AddGoFlagSet(zapFS)
	return cmd
}

var setupLog = ctrl.Log.WithName("setup")

func runManager(ctx context.Context, cfg *config.Config, zapOpts *zap.Options) error {
	scheme := buildScheme()

	obs, err := observability.Setup(ctx, cfg.Otel)
	if err != nil {
		return fmt.Errorf("setup observability: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutdownCtx)
	}()

	ctrl.SetLogger(buildLogger(zapOpts, obs.LogCore))

	tlsOpts := buildTLSOpts(cfg.HTTP2)
	webhookServer := newWebhookServer(cfg.Webhook, tlsOpts)
	metricsServerOpts := newMetricsOpts(cfg.Metrics, tlsOpts)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOpts,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.Health.ProbeBindAddress,
		LeaderElection:         cfg.Leader.Enabled,
		LeaderElectionID:       "ab382048.egoavara.net",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := registerControllers(mgr, cfg); err != nil {
		return err
	}

	if err := registerAPIServer(mgr, cfg.API, cfg.Otel, obs); err != nil {
		return err
	}

	if err := registerPreflight(mgr); err != nil {
		return err
	}

	if err := startStandalonePromListener(ctx, cfg.Otel, obs); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}

func buildScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(routeprismv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
	return scheme
}

// buildLogger applies zap.Options the way controller-runtime's zap.New
// does, then optionally tees the resulting core into the OTel log bridge
// so log records flow to both stderr and the collector.
func buildLogger(zapOpts *zap.Options, otelCore zapcore.Core) logr.Logger {
	if otelCore == nil {
		return zap.New(zap.UseFlagOptions(zapOpts))
	}
	raw := zap.NewRaw(zap.UseFlagOptions(zapOpts)).WithOptions(
		zaplog.WrapCore(func(c zapcore.Core) zapcore.Core {
			return zapcore.NewTee(c, otelCore)
		}),
	)
	return zapr.NewLogger(raw)
}

// disableHTTP2 mitigates HTTP/2 stream cancellation / rapid reset CVEs
// when HTTP/2 is intentionally turned off.
func buildTLSOpts(http2Enabled bool) []func(*tls.Config) {
	if http2Enabled {
		return nil
	}
	return []func(*tls.Config){
		func(c *tls.Config) {
			setupLog.Info("Disabling HTTP/2")
			c.NextProtos = []string{"http/1.1"}
		},
	}
}

func newWebhookServer(cfg config.Webhook, tlsOpts []func(*tls.Config)) webhook.Server {
	opts := webhook.Options{TLSOpts: tlsOpts}
	if cfg.CertPath != "" {
		setupLog.Info("Initializing webhook certificate watcher",
			"webhook-cert-path", cfg.CertPath, "webhook-cert-name", cfg.CertName, "webhook-cert-key", cfg.CertKey)
		opts.CertDir = cfg.CertPath
		opts.CertName = cfg.CertName
		opts.KeyName = cfg.CertKey
	}
	return webhook.NewServer(opts)
}

func newMetricsOpts(cfg config.Metrics, tlsOpts []func(*tls.Config)) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   cfg.BindAddress,
		SecureServing: cfg.Secure,
		TLSOpts:       tlsOpts,
	}
	if cfg.Secure {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if cfg.CertPath != "" {
		setupLog.Info("Initializing metrics certificate watcher",
			"metrics-cert-path", cfg.CertPath, "metrics-cert-name", cfg.CertName, "metrics-cert-key", cfg.CertKey)
		opts.CertDir = cfg.CertPath
		opts.CertName = cfg.CertName
		opts.KeyName = cfg.CertKey
	}
	return opts
}

func registerControllers(mgr ctrl.Manager, cfg *config.Config) error {
	if err := (&controller.ContextRouteReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("contextroute-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ContextRoute controller: %w", err)
	}
	if err := (&controller.EdgeTransformationReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorderFor("edgetransformation-controller"),
		OperatorAPIAddr: cfg.API.AdvertisedAddress,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup EdgeTransformation controller: %w", err)
	}
	// +kubebuilder:scaffold:builder
	return nil
}

func registerAPIServer(mgr ctrl.Manager, cfg config.API, otelCfg config.Otel, obs observability.Result) error {
	apiOpts := apiserver.Options{
		BindAddress: cfg.BindAddress,
		// Only wrap the router with otelhttp when traces are actually being
		// exported — otherwise the wrapper just adds overhead for no signal.
		Instrument:  otelCfg.Traces.Enabled,
		MetricsPath: otelCfg.Prometheus.Path,
	}
	// Mount the prom handler on the API server when no dedicated Prometheus
	// listener was configured. Otherwise it runs on its own port.
	if obs.PromHandler != nil && otelCfg.Prometheus.BindAddress == "" {
		apiOpts.PromHandler = obs.PromHandler
	}

	if err := mgr.Add(apiserver.New(mgr.GetClient(), mgr.GetCache(), apiOpts)); err != nil {
		return fmt.Errorf("register API server: %w", err)
	}
	return nil
}

func registerPreflight(mgr ctrl.Manager) error {
	preflightOpts := preflight.Options{
		Namespace: os.Getenv("POD_NAMESPACE"),
	}
	if podName := os.Getenv("POD_NAME"); podName != "" && preflightOpts.Namespace != "" {
		preflightOpts.SelfPod = &corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       podName,
			Namespace:  preflightOpts.Namespace,
			UID:        types.UID(os.Getenv("POD_UID")),
		}
	}
	if err := mgr.Add(preflight.New(
		mgr.GetConfig(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("route-prism-preflight"),
		preflightOpts,
	)); err != nil {
		return fmt.Errorf("register preflight runner: %w", err)
	}
	return nil
}

// startStandalonePromListener handles the case where the operator wants
// /metrics on a separate port (e.g. when the API server is disabled, or
// when network policy treats Prometheus traffic differently).
func startStandalonePromListener(ctx context.Context, cfg config.Otel, obs observability.Result) error {
	if !cfg.Prometheus.Enabled || obs.PromHandler == nil || cfg.Prometheus.BindAddress == "" {
		return nil
	}
	path := cfg.Prometheus.Path
	if path == "" {
		path = "/metrics"
	}
	mux := http.NewServeMux()
	mux.Handle(path, obs.PromHandler)
	srv := &http.Server{
		Addr:              cfg.Prometheus.BindAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		setupLog.Info("Starting Prometheus listener", "addr", cfg.Prometheus.BindAddress, "path", path)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "Prometheus listener failed")
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return nil
}
