/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License").
*/

// sample-tier — minimal HTTP service used by the devloop sample's 3-tier
// chain (web → api → db). The chain endpoint at /api returns this tier's
// identity (tier name + pod hostname + the Baggage / Cookie it received)
// and, when NEXT_HOP is set, calls <NEXT_HOP>/api with the same Baggage
// and Cookie headers and embeds that response under .downstream — yielding
// a nested JSON tree that records every hop the request traversed.
//
// When SERVE_PAGE=true the root path additionally serves a tiny HTML
// console (one button to fire /api, plus the route-prism widget loaded
// via /<PATH_PREFIX>/widget.js). The /<PATH_PREFIX>/* path is reverse-
// proxied to OPERATOR_URL so the widget bundle and its /api/v1/* calls
// resolve even when the user reaches the pod via direct port-forward
// (which bypasses the mesh-attached HTTPRoute that would normally route
// the prefix to the operator).
//
// Env:
//
//	TIER          short label included in the response (default: "unknown")
//	VARIANT       deployment variant label (e.g. "stable" / "web-canary").
//	              Echoed as `.variant` so chain consumers can tell which
//	              variant served each hop without decoding pod hashes.
//	NEXT_HOP      downstream URL (e.g. http://api). Omit on the leaf tier.
//	PORT          listen port (default: 8080 — distroless nonroot can't bind <1024)
//	SERVE_PAGE    "true" → render the HTML console at "/" (default: JSON chain)
//	PATH_PREFIX   widget path prefix WITHOUT leading slash (e.g. ".route-prism")
//	OPERATOR_URL  operator API base, used to reverse-proxy /<PATH_PREFIX>/*
//	              (e.g. http://route-prism-controller-manager-api.route-prism-system:8082)
//	ROUTING_KEY   widget config: this tier's routing key (e.g. "demo.web")
//	SOURCE_COOKIE widget config: cookie name (e.g. "x-route-prism")
//
// Image: route-prism-sample-tier:latest (built by the Tiltfile from
// bin/sample-tier-linux compiled on the host).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

type chainResponse struct {
	Tier       string `json:"tier"`
	Variant    string `json:"variant,omitempty"`
	Pod        string `json:"pod"`
	BaggageIn  string `json:"baggage_in,omitempty"`
	CookieIn   string `json:"cookie_in,omitempty"`
	Downstream any    `json:"downstream,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	tier := getenv("TIER", "unknown")
	variant := os.Getenv("VARIANT")
	nextHop := os.Getenv("NEXT_HOP")
	port := getenv("PORT", "8080")
	pod, _ := os.Hostname()
	servePage := os.Getenv("SERVE_PAGE") == "true"
	pathPrefix := strings.Trim(os.Getenv("PATH_PREFIX"), "/")
	operatorURL := os.Getenv("OPERATOR_URL")
	routingKey := os.Getenv("ROUTING_KEY")
	sourceCookie := os.Getenv("SOURCE_COOKIE")
	// CORS_ALLOW_ORIGINS — comma-separated origins. When set, the tier
	// answers preflight OPTIONS and stamps Access-Control-Allow-* on
	// every response. Required for the demo's cross-origin button
	// (web at :8080 calling api at :8081 directly from the browser).
	corsOrigins := splitNonEmpty(os.Getenv("CORS_ALLOW_ORIGINS"))
	// CROSS_API_URL — absolute URL the demo page's "Call api directly"
	// button hits. Empty hides the button.
	crossAPIURL := os.Getenv("CROSS_API_URL")

	mux := http.NewServeMux()

	chainHandler := func(w http.ResponseWriter, r *http.Request) {
		resp := chainResponse{
			Tier:      tier,
			Variant:   variant,
			Pod:       pod,
			BaggageIn: r.Header.Get("Baggage"),
			CookieIn:  r.Header.Get("Cookie"),
		}
		if nextHop != "" {
			down, err := callDownstream(r.Context(), nextHop+"/api", r.Header)
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
	}
	mux.HandleFunc("/api", chainHandler)

	// Widget reverse-proxy. Direct port-forward bypasses the mesh
	// HTTPRoute that normally forwards /<pathPrefix>/* to the operator,
	// so we proxy here instead — this is what lets the embedded widget
	// load its bundle + call /api/v1/* without leaving the demo pod.
	if servePage && pathPrefix != "" && operatorURL != "" {
		target, err := url.Parse(operatorURL)
		if err != nil {
			log.Fatalf("OPERATOR_URL invalid: %v", err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		mux.Handle("/"+pathPrefix+"/", http.StripPrefix("/"+pathPrefix, proxy))
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if servePage {
			renderPage(w, tier, variant, pod, pathPrefix, routingKey, sourceCookie, crossAPIURL)
			return
		}
		// Backward-compat: tiers without SERVE_PAGE answer "/" with the
		// chain JSON so existing curl-based smoke checks keep working.
		chainHandler(w, r)
	})

	var handler http.Handler = mux
	if len(corsOrigins) > 0 {
		handler = corsMiddleware(corsOrigins, mux)
	}

	addr := ":" + port
	log.Printf("sample-tier %s/%s listening on %s (next_hop=%q, page=%v, cors=%v, pod=%s)",
		tier, variant, addr, nextHop, servePage, corsOrigins, pod)
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

// corsMiddleware echoes the request's Origin back when it's on the allow
// list and answers OPTIONS preflights with 204. Allow-Headers covers the
// W3C Baggage header the widget's fetch / XHR override stamps so the
// browser permits it on cross-origin calls. Credentials are enabled so
// the demo can also send cookies on same-domain testing.
func corsMiddleware(allow []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && originAllowed(origin, allow) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Baggage, Content-Type, Cookie")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin string, allow []string) bool {
	for _, a := range allow {
		if a == "*" || a == origin {
			return true
		}
	}
	return false
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// callDownstream issues a GET to <url> with the inbound Baggage + Cookie
// headers copied verbatim, then unmarshals the JSON response into a
// generic value so it can be embedded as nested JSON in our own output.
func callDownstream(ctx context.Context, target string, in http.Header) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
		return string(body), nil
	}
	return parsed, nil
}

func renderPage(w http.ResponseWriter, tier, variant, pod, pathPrefix, routingKey, sourceCookie, crossAPIURL string) {
	cfg := map[string]any{
		"target":       tier,
		"routingKey":   routingKey,
		"sourceCookie": sourceCookie,
		"pathPrefix":   pathPrefix,
		"style": map[string]any{
			"mode":   "float",
			"anchor": "bottom-right",
			"margin": map[string]string{"bottom": "16px", "right": "16px"},
		},
		// fetch / xhr override on. The widget will stamp Baggage on
		// every outbound call so the cross-origin button below works
		// without relying on the SourceCookie (which the browser drops
		// on cross-origin requests).
		"js": map[string]any{
			"fetch":          map[string]any{"enable": true},
			"xmlhttprequest": map[string]any{"enable": true},
		},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "config encode failed", http.StatusInternalServerError)
		return
	}
	// Single-quote the data attribute and escape ' so JSON containing
	// apostrophes can't break out of the attribute.
	cfgAttr := strings.ReplaceAll(string(cfgJSON), "'", "&#39;")
	// Mirrors the translator's nginx convention: widget bundle lives at
	// /<pathPrefix>/widget/widget.js, /api/v1/* under /<pathPrefix>/api/.
	widgetSrc := "/" + pathPrefix + "/widget/widget.js"

	displayVariant := variant
	if displayVariant == "" {
		displayVariant = "(unset)"
	}
	// crossAPIURL is JSON-encoded so the JS literal stays well-formed
	// even when the env is empty (in which case both cross-origin
	// buttons render but are disabled).
	crossURLJSON, _ := json.Marshal(crossAPIURL)
	crossDisabled := ""
	crossLabelSuffix := crossAPIURL
	if crossAPIURL == "" {
		crossDisabled = " disabled"
		crossLabelSuffix = "(set CROSS_API_URL)"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, pageTemplate,
		tier, tier, displayVariant, pod,
		crossDisabled, crossLabelSuffix, // cross-fetch
		crossDisabled, crossLabelSuffix, // cross-xhr
		string(crossURLJSON), // JS const for cross URL
		widgetSrc, cfgAttr,
	)
}

// %s order: <title> tier, header tier, header variant, pod,
//
//	cross-fetch [disabled?] attr, cross-fetch label suffix,
//	cross-xhr   [disabled?] attr, cross-xhr   label suffix,
//	cross-URL JSON literal, widget src, widget cfg attr.
const pageTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>route-prism demo — %s</title>
  <style>
    :root { color-scheme: light; }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
      color: #1c1c1c;
      background: #f4f5f7;
      line-height: 1.45;
    }
    header {
      background: #1f2937;
      color: #f9fafb;
      padding: 1.25rem 1.75rem;
    }
    header h1 { margin: 0 0 0.4rem; font-size: 1.15rem; letter-spacing: 0.01em; }
    header .meta {
      display: flex; flex-wrap: wrap; gap: 0.85rem;
      font-size: 0.78rem; color: #cbd5e1;
    }
    header code {
      font: 0.78rem/1 ui-monospace, SFMono-Regular, Menlo, monospace;
      background: rgba(255,255,255,0.08);
      padding: 0.1rem 0.4rem;
      border-radius: 4px;
    }
    main {
      max-width: 760px; margin: 1.5rem auto; padding: 0 1.25rem 6rem;
    }
    section {
      background: #fff; border: 1px solid #e5e7eb; border-radius: 8px;
      padding: 1.1rem 1.25rem; margin-bottom: 1.25rem;
      box-shadow: 0 1px 2px rgba(15,23,42,0.04);
    }
    section h2 {
      margin: 0 0 0.6rem; font-size: 0.92rem;
      letter-spacing: 0.04em; text-transform: uppercase;
      color: #475569;
    }
    .row { display: flex; gap: 0.6rem; align-items: center; flex-wrap: wrap; }
    button.primary {
      background: #2563eb; color: #fff; border: 0; padding: 0.5rem 0.95rem;
      font: 0.85rem/1 inherit; border-radius: 6px; cursor: pointer;
      transition: background 120ms ease;
    }
    button.primary:hover { background: #1d4ed8; }
    button.primary:disabled { background: #94a3b8; cursor: not-allowed; }
    button.primary.secondary { background: #475569; }
    button.primary.secondary:hover { background: #334155; }
    .hint { font-size: 0.78rem; color: #64748b; }
    pre.result {
      margin: 0.85rem 0 0; padding: 0.85rem 1rem;
      background: #0f172a; color: #e2e8f0;
      border-radius: 6px; font-size: 0.78rem;
      max-height: 400px; overflow: auto;
      font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    }
    pre.result.empty { color: #64748b; background: #f1f5f9; }
  </style>
</head>
<body>
  <header>
    <h1>route-prism demo · <code>%s</code> tier</h1>
    <div class="meta">
      <span>variant <code>%s</code></span>
      <span>pod <code>%s</code></span>
      <span>open the widget at the bottom-right to set route variants per tier</span>
    </div>
  </header>
  <main>
    <section>
      <h2>same-origin chain</h2>
      <div class="row">
        <button id="same-fetch" class="primary">fetch /api</button>
        <button id="same-xhr"   class="primary secondary">XHR /api</button>
        <span class="hint">GET /api on this tier; chains through next hop. Cookie carries the routing context.</span>
      </div>
      <pre id="result-same" class="result empty">click a button to run a chained request</pre>
    </section>
    <section>
      <h2>cross-origin direct call</h2>
      <div class="row">
        <button id="cross-fetch" class="primary"%s>fetch %s</button>
        <button id="cross-xhr"   class="primary secondary"%s>XHR %s</button>
        <span class="hint">GET on a different origin (cookies are dropped). The widget's fetch / XHR override stamps the W3C Baggage header so the receiving translator still routes by variant.</span>
      </div>
      <pre id="result-cross" class="result empty">click a button to issue a direct cross-origin GET</pre>
    </section>
    <section>
      <h2>cookie inspector</h2>
      <pre id="cookie" class="result empty">document.cookie will appear here after each call</pre>
    </section>
  </main>
  <script>
    (function () {
      var CROSS_URL = %s;
      var ck = document.getElementById('cookie');
      function refreshCookie() {
        ck.textContent = document.cookie || '(no cookies)';
        ck.classList.toggle('empty', !document.cookie);
      }
      refreshCookie();

      // viaFetch / viaXHR each return a Promise resolving to
      // {status, statusText, body, transport}. Keeping the shape
      // uniform lets a single render function handle either path.
      function viaFetch(url) {
        return fetch(url, { headers: { 'Accept': 'application/json' } })
          .then(function (r) {
            return r.text().then(function (body) {
              return { status: r.status, statusText: r.statusText, body: body, transport: 'fetch' };
            });
          });
      }
      function viaXHR(url) {
        return new Promise(function (resolve, reject) {
          var xhr = new XMLHttpRequest();
          xhr.open('GET', url, true);
          xhr.setRequestHeader('Accept', 'application/json');
          // No withCredentials — match the fetch path so the cross-origin
          // demo really exercises the Baggage header path, not cookies.
          xhr.onload = function () {
            resolve({
              status: xhr.status,
              statusText: xhr.statusText,
              body: xhr.responseText,
              transport: 'xhr',
            });
          };
          xhr.onerror = function () { reject(new Error('XHR network error')); };
          xhr.send();
        });
      }

      function wire(btnId, outId, run) {
        var btn = document.getElementById(btnId);
        var out = document.getElementById(outId);
        if (!btn || btn.disabled) return;
        btn.addEventListener('click', async function () {
          btn.disabled = true;
          out.classList.remove('empty');
          out.textContent = 'calling ...';
          try {
            var t0 = performance.now();
            var r = await run();
            var dt = (performance.now() - t0).toFixed(0);
            var pretty = r.body;
            try { pretty = JSON.stringify(JSON.parse(r.body), null, 2); } catch (_) {}
            out.textContent = '// [' + r.transport + '] '
              + r.status + ' ' + r.statusText + ' (' + dt + 'ms)\n' + pretty;
          } catch (e) {
            out.textContent = 'error: ' + (e && e.message || e);
          } finally {
            refreshCookie();
            btn.disabled = false;
          }
        });
      }

      wire('same-fetch', 'result-same', function () { return viaFetch('/api'); });
      wire('same-xhr',   'result-same', function () { return viaXHR('/api'); });
      if (CROSS_URL) {
        wire('cross-fetch', 'result-cross', function () { return viaFetch(CROSS_URL); });
        wire('cross-xhr',   'result-cross', function () { return viaXHR(CROSS_URL); });
      }
    })();
  </script>
  <script src="%s" data-route-prism-config='%s' defer></script>
</body>
</html>
`

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
