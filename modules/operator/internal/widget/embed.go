/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package widget serves the in-page routing widget bundle that
// EdgeTransformation can inject into HTML responses. The compiled Vite
// (Preact) bundle is baked into the binary via embed.FS so the operator
// ships as a single artifact.
package widget

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the dist/ subtree as a root-rebased fs.FS suitable for
// http.FileServer.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
