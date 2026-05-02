/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package test implements a tiny self-identifying HTTP server used as the
// backend for `route-prism verify` deep probes. It deliberately has no
// dependency on the controller code so it stays trivial to ship as part
// of the operator image — the same binary serves both `run` and `test`.
package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Identity is what a single test backend reports about itself on every
// request. Stable JSON shape so the verify orchestrator can match by a
// known field instead of substring-grepping a free-form body.
type Identity struct {
	Name    string              `json:"name"`
	Host    string              `json:"host"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Baggage string              `json:"baggage"`
	Cookie  string              `json:"cookie"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// Options configures Serve.
type Options struct {
	Addr           string // listen address (host:port)
	Name           string // identity reported as `name`
	IncludeHeaders bool   // when true, echo all request headers
}

// Serve starts the HTTP server. It blocks until the listener errors.
func Serve(opts Options) error {
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}
	if opts.Name == "" {
		opts.Name = strings.TrimSpace(os.Getenv("VARIANT_NAME"))
	}
	if opts.Name == "" {
		opts.Name = strings.TrimSpace(os.Getenv("HOSTNAME"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		id := Identity{
			Name:    opts.Name,
			Host:    host,
			Method:  r.Method,
			Path:    r.URL.Path,
			Baggage: r.Header.Get("Baggage"),
			Cookie:  r.Header.Get("Cookie"),
		}
		if opts.IncludeHeaders {
			id.Headers = map[string][]string(r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Variant-Name", id.Name)
		_ = json.NewEncoder(w).Encode(id)
	})

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}
