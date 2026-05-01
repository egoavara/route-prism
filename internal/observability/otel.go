/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package observability owns the optional OpenTelemetry pipeline. Setup is
// idempotent and safe to skip — when disabled, the global tracer / meter /
// logger providers stay as no-ops and the rest of the codebase keeps using
// otel.Tracer / otel.Meter / global.Logger the same way.
//
// Each signal (traces, metrics, logs) is opt-in and exported via OTLP/gRPC.
// Metrics additionally support a Prometheus pull exporter that publishes
// the same instruments at /metrics — useful when the platform already has
// a Prometheus stack and you want OTel-native instrumentation alongside.
package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.uber.org/zap/zapcore"

	"github.com/egoavara/route-prism/internal/config"
)

// ShutdownFunc flushes and closes the providers. Always call it on exit;
// it is a no-op when Setup returned without installing anything.
type ShutdownFunc func(context.Context) error

// Result is what Setup hands back to the caller: a shutdown hook plus
// optional plumbing the caller should wire up (Prometheus handler, zap
// log core that bridges into OTel logs).
type Result struct {
	Shutdown ShutdownFunc
	// PromHandler is non-nil only when cfg.Prometheus.Enabled is true.
	// Mount it on whatever HTTP surface is appropriate (typically the API
	// server's chi router at /metrics).
	PromHandler http.Handler
	// LogCore is non-nil only when cfg.Logs is true. Wrap your zap logger
	// with it (e.g. via zapcore.NewTee) so log records are forked into the
	// OTel log pipeline in addition to stderr.
	LogCore zapcore.Core
}

// Setup installs trace/metric/log providers based on cfg. Returning a
// no-op shutdown when nothing is enabled keeps callers branchless.
//
// Each signal has its own Enabled toggle — there is no master switch,
// just flip the signals you want. `--otel.traces.enabled=true` is
// sufficient on its own.
func Setup(ctx context.Context, cfg config.Otel) (Result, error) {
	if !cfg.Traces.Enabled && !cfg.Metrics.Enabled && !cfg.Logs.Enabled && !cfg.Prometheus.Enabled {
		return Result{Shutdown: func(context.Context) error { return nil }}, nil
	}

	res, err := buildResource(ctx, cfg.ServiceName)
	if err != nil {
		return Result{}, fmt.Errorf("build otel resource: %w", err)
	}

	out := Result{}
	var shutdowns []ShutdownFunc

	if cfg.Traces.Enabled {
		tp, err := newTracerProvider(ctx, res, cfg)
		if err != nil {
			_ = runShutdown(ctx, shutdowns)
			return Result{}, err
		}
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	if cfg.Metrics.Enabled || cfg.Prometheus.Enabled {
		mp, promHandler, err := newMeterProvider(ctx, res, cfg)
		if err != nil {
			_ = runShutdown(ctx, shutdowns)
			return Result{}, err
		}
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
		if promHandler != nil {
			out.PromHandler = promHandler
		}
	}

	if cfg.Logs.Enabled {
		lp, err := newLoggerProvider(ctx, res, cfg)
		if err != nil {
			_ = runShutdown(ctx, shutdowns)
			return Result{}, err
		}
		otellog.SetLoggerProvider(lp)
		shutdowns = append(shutdowns, lp.Shutdown)
		out.LogCore = otelzap.NewCore("github.com/egoavara/route-prism",
			otelzap.WithLoggerProvider(lp),
		)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	out.Shutdown = func(ctx context.Context) error { return runShutdown(ctx, shutdowns) }
	return out, nil
}

func buildResource(ctx context.Context, service string) (*resource.Resource, error) {
	if service == "" {
		service = "route-prism"
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(service)),
	)
}

func newTracerProvider(ctx context.Context, res *resource.Resource, cfg config.Otel) (*sdktrace.TracerProvider, error) {
	opts := []otlptracegrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

// newMeterProvider builds a single meter provider that fans out to every
// reader the operator opted into: an OTLP push reader (when cfg.Metrics)
// and/or a Prometheus pull exporter (when cfg.Prometheus.Enabled). Sharing
// one provider keeps instrument identities consistent across exporters.
func newMeterProvider(ctx context.Context, res *resource.Resource, cfg config.Otel) (*sdkmetric.MeterProvider, http.Handler, error) {
	var readers []sdkmetric.Reader
	var promHandler http.Handler

	if cfg.Metrics.Enabled {
		opts := []otlpmetricgrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("create otlp metric exporter: %w", err)
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(exp))
	}

	if cfg.Prometheus.Enabled {
		// otelprom.New returns a Reader that doubles as a prometheus.Collector
		// — registering it in a Registry lets promhttp expose its instruments.
		promExp, err := otelprom.New()
		if err != nil {
			return nil, nil, fmt.Errorf("create prometheus exporter: %w", err)
		}
		readers = append(readers, promExp)
		promHandler = promhttp.Handler()
	}

	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		mpOpts = append(mpOpts, sdkmetric.WithReader(r))
	}
	return sdkmetric.NewMeterProvider(mpOpts...), promHandler, nil
}

func newLoggerProvider(ctx context.Context, res *resource.Resource, cfg config.Otel) (*sdklog.LoggerProvider, error) {
	opts := []otlploggrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlploggrpc.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	exp, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp log exporter: %w", err)
	}
	return sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	), nil
}

func runShutdown(ctx context.Context, fns []ShutdownFunc) error {
	var errs []error
	for _, fn := range fns {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
