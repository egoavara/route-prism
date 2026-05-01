/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package widget

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// Handler returns an http.Handler that serves the embedded widget bundle.
//
//   - GET /widget.js               → IIFE bundle (Preact + UI)
//   - GET /widget.css              → optional split CSS (only if vite emits it)
//   - GET /preview                 → small HTML page that loads widget.js
//     with a data-route-prism-config built
//     from query-string params. The dashboard
//     embeds this in an <iframe> to show a
//     live preview.
//   - GET /<other>                 → static file under dist/ if present
//
// The translator (with widgetInjection enabled) proxies its configured
// pathPrefix to this handler so the widget bundle is fetched on the host
// page's origin (no CORS).
//
// Callers mount this under "/widget" using http.StripPrefix.
func Handler() http.Handler {
	root := FS()
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "preview" || path == "preview/" {
			servePreview(w, r)
			return
		}
		if path == "" {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(root, path); err != nil {
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// servePreview renders a minimal HTML host page that loads the widget
// with options taken verbatim from the query string. Designed for the
// dashboard's preview iframe; safe enough to share publicly because every
// option flows back into a dataset attribute (no script injection).
func servePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cfg := map[string]any{
		"pathPrefix":   firstNonEmpty(q.Get("pathPrefix"), ".route-prism"),
		"target":       q.Get("target"),
		"routingKey":   q.Get("routingKey"),
		"sourceCookie": q.Get("sourceCookie"),
		"style": map[string]any{
			"mode":   firstNonEmpty(q.Get("style.mode"), "float"),
			"anchor": q.Get("style.anchor"),
			"margin": map[string]string{
				"top":    q.Get("style.margin.top"),
				"right":  q.Get("style.margin.right"),
				"bottom": q.Get("style.margin.bottom"),
				"left":   q.Get("style.margin.left"),
			},
			"hotkey": map[string]string{
				"open": q.Get("style.hotkey.open"),
			},
		},
		// Preview mode: the widget should treat /api/v1/* as same-origin
		// rather than prefixed (operator origin already serves it).
		"previewMode": true,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "config encode failed", http.StatusInternalServerError)
		return
	}
	// Single-quote the data attribute and HTML-escape ' so JSON containing
	// apostrophes doesn't break out.
	cfgAttr := strings.ReplaceAll(string(cfgJSON), "'", "&#39;")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>route-prism widget preview</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%%; background: #f5f5f5;
                 font-family: system-ui, -apple-system, sans-serif; }
    .panel { padding: 2rem; max-width: 32rem; margin: 0 auto; color: #333; }
    .panel h1 { font-size: 1rem; font-weight: 600; }
    .panel p { font-size: 0.875rem; line-height: 1.5; color: #666; }
  </style>
</head>
<body>
  <div class="panel">
    <h1>Widget preview</h1>
    <p>This frame mounts the widget exactly the way an end-user page would.
       Use the dashboard controls on the left to tweak style options;
       this frame reloads to reflect them.</p>
  </div>
  <script src="/widget/widget.js" data-route-prism-config='%s' defer></script>
</body>
</html>`, cfgAttr)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
