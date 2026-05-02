/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package cmd

import (
	"github.com/spf13/cobra"

	testbackend "github.com/egoavara/route-prism/internal/test"
)

func newTestCmd() *cobra.Command {
	var (
		addr           string
		name           string
		includeHeaders bool
	)
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Run a tiny self-identifying HTTP echo backend (used by `verify`)",
		Long: `Starts an HTTP server that responds with a JSON document describing
itself: configured name (or VARIANT_NAME env / hostname fallback),
the request method/path, and the inbound baggage / cookie headers.

This is the backend image used by ` + "`route-prism verify`" + ` for end-to-end
HTTPRoute traffic verification, but it is also useful as a generic mesh
debugging endpoint.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return testbackend.Serve(testbackend.Options{
				Addr:           addr,
				Name:           name,
				IncludeHeaders: includeHeaders,
			})
		},
	}
	pf := cmd.Flags()
	pf.StringVar(&addr, "addr", ":8080", "Listen address (host:port)")
	pf.StringVar(&name, "name", "", "Identity name; defaults to $VARIANT_NAME, then $HOSTNAME")
	pf.BoolVar(&includeHeaders, "include-headers", false, "Include the full set of request headers in every response")
	return cmd
}
