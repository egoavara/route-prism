/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package main

import (
	"os"

	"github.com/egoavara/route-prism/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
