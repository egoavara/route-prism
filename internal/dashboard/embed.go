/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

// Package dashboard serves the read-only web UI that visualizes the
// routing surface exposed by the apiserver. The compiled Vite bundle is
// baked into the binary via embed.FS so the controller ships as a
// single artifact.
package dashboard

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
