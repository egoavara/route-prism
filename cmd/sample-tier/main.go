/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License").
*/

// sample-tier — minimal HTTP service used by the devloop sample's 3-tier
// chain (web → api → db). On every GET it returns its own identity (tier
// name + pod hostname + the Baggage / Cookie it received) and, when a
// NEXT_HOP env is set, calls the next tier with the same Baggage and
// Cookie headers and embeds that response under .downstream — producing a
// nested JSON tree that records every hop the request traversed.
//
// Env:
//   TIER       short label included in the response (default: "unknown")
//   NEXT_HOP   downstream URL (e.g. http://api). Omit on the leaf tier.
//   PORT       listen port (default: 8080 — distroless nonroot can't bind <1024)
//
// Image: route-prism-sample-tier:latest (built by the Tiltfile from
// bin/sample-tier-linux compiled on the host).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type chainResponse struct {
	Tier       string `json:"tier"`
	Pod        string `json:"pod"`
	BaggageIn  string `json:"baggage_in,omitempty"`
	CookieIn   string `json:"cookie_in,omitempty"`
	Downstream any    `json:"downstream,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	tier := getenv("TIER", "unknown")
	nextHop := os.Getenv("NEXT_HOP")
	port := getenv("PORT", "8080")
	pod, _ := os.Hostname()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp := chainResponse{
			Tier:      tier,
			Pod:       pod,
			BaggageIn: r.Header.Get("Baggage"),
			CookieIn:  r.Header.Get("Cookie"),
		}
		if nextHop != "" {
			down, err := callDownstream(r.Context(), nextHop, r.Header)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Downstream = down
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("encode response: %v", err)
		}
	})

	addr := ":" + port
	log.Printf("sample-tier %s listening on %s (next_hop=%q, pod=%s)", tier, addr, nextHop, pod)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

// callDownstream issues a GET to <url>/ with the inbound Baggage + Cookie
// headers copied verbatim, then unmarshals the JSON response into a
// generic value so it can be embedded as nested JSON in our own output.
func callDownstream(ctx context.Context, url string, in http.Header) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Baggage", "Cookie"} {
		if v := in.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Downstream didn't return JSON — surface the raw body as a string
		// so the chain still renders sensibly.
		return string(body), nil
	}
	return parsed, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
