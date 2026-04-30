/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package apiserver exposes a small read-only HTTP API that surfaces
// the east-west routing surface managed by route-prism: which Services
// are addressable as routing targets, and which alternatives (variants)
// each target can be diverted to via Baggage.
package apiserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// rebuildDebounce is the window over which informer events are coalesced
// into a single snapshot rebuild. Long enough to swallow the burst that
// fires during initial cache sync, short enough that a human edit is
// reflected before they refresh the UI.
const rebuildDebounce = 100 * time.Millisecond

// Options configures the API server.
type Options struct {
	// BindAddress is the listen address (e.g. ":8082"). Empty disables the server.
	BindAddress string
}

// Server is a manager.Runnable that:
//   - registers informer event handlers to keep the routing-surface Index fresh
//   - serves the route-prism HTTP API from that Index (lock-free reads)
type Server struct {
	idx     *Index
	api     *API
	cache   ctrlcache.Cache
	options Options
}

// New constructs an API server. The cache must be the same one the
// manager owns (mgr.GetCache()) so informer event registration is
// authoritative; the client should be the cache-backed client.
func New(c client.Client, cache ctrlcache.Cache, opts Options) *Server {
	idx := NewIndex(c)
	return &Server{
		idx:     idx,
		api:     NewAPI(idx),
		cache:   cache,
		options: opts,
	}
}

// NeedLeaderElection ensures the API server runs on every replica, not
// only on the leader — clients should be able to reach any Pod.
func (s *Server) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable. The manager guarantees the cache is
// synced before this runs, so the initial Rebuild and informer
// registrations see a populated store immediately.
func (s *Server) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("apiserver")

	deb := newDebouncer(rebuildDebounce, func() {
		// Use a detached context — the informer fires events outside the
		// request scope and we don't want a request cancellation to cancel
		// a rebuild.
		if err := s.idx.Rebuild(context.Background()); err != nil {
			log.Error(err, "Failed to rebuild routing index")
		}
	})
	defer deb.stop()

	if err := s.registerEventHandlers(ctx, deb, log); err != nil {
		return err
	}
	if err := s.idx.Rebuild(ctx); err != nil {
		return err
	}

	if s.options.BindAddress == "" {
		log.Info("API server disabled (empty bind address); index still maintained")
		<-ctx.Done()
		return nil
	}

	mux := http.NewServeMux()
	s.api.Register(mux)
	srv := &http.Server{
		Addr:              s.options.BindAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("Starting API server", "addr", s.options.BindAddress)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// registerEventHandlers wires the cache informers for ContextRoute and
// Service to trigger a (debounced) snapshot rebuild on any change. We
// intentionally don't try to compute a partial diff: the dataset is
// small, and a full rebuild keeps the snapshot internally consistent
// (owner resolution, dedup, sorted parallel slices).
func (s *Server) registerEventHandlers(ctx context.Context, deb *debouncer, log logr) error {
	crInf, err := s.cache.GetInformer(ctx, &routeprismv1alpha1.ContextRoute{})
	if err != nil {
		return err
	}
	if _, err := crInf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { deb.trigger() },
		UpdateFunc: func(_, _ any) { deb.trigger() },
		DeleteFunc: func(_ any) { deb.trigger() },
	}); err != nil {
		return err
	}

	svcInf, err := s.cache.GetInformer(ctx, &corev1.Service{})
	if err != nil {
		return err
	}
	if _, err := svcInf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { deb.trigger() },
		UpdateFunc: func(_, _ any) { deb.trigger() },
		DeleteFunc: func(_ any) { deb.trigger() },
	}); err != nil {
		return err
	}
	log.Info("Registered informer event handlers for routing index")
	return nil
}

// logr is the minimal logging surface we need; lets us avoid importing
// the full go-logr API into the function signature.
type logr interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

// compile-time check.
var _ manager.Runnable = (*Server)(nil)
