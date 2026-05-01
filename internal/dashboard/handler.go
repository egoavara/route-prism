/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package dashboard

import (
	"io/fs"
	"net/http"
	"strings"
)

// Handler returns an http.Handler that serves the embedded dashboard.
// Unknown paths fall back to index.html so SPA-style client routing
// keeps working when added later.
//
// Callers are expected to mount this under a prefix (e.g. "/dashboard")
// using http.StripPrefix — the handler treats the request path as
// relative to the dist root.
func Handler() http.Handler {
	root := FS()
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(root, path); err != nil {
			// SPA fallback. Write index.html directly instead of
			// rewriting the request — http.FileServer canonicalizes
			// "/index.html" requests to "./", which loops once the
			// path has been stripped of its mount prefix.
			b, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
