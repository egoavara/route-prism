/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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
