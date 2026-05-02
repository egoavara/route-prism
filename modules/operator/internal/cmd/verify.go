/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/egoavara/route-prism/internal/preflight"
	"github.com/egoavara/route-prism/internal/verify"
)

func newVerifyCmd() *cobra.Command {
	var (
		kubeconfigPath string
		contextName    string
		namespace      string
		timeout        time.Duration
		keepNamespace  bool
		noTUI          bool
		testImage      string
		meshMode       string
		onExisting     string
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check that the target cluster has GAMMA support working",
		Long: `Probe a cluster — interactively or in CI — to confirm that:
 • The Gateway API HTTPRoute CRD is installed.
 • A GAMMA-aware service mesh is present (Istio, Cilium, …).
 • An HTTPRoute with a Service parentRef is actually accepted.

Without --context the command launches a TUI that lists every
context in the merged kubeconfig and lets you pick one. With
--context (or --no-tui) it runs non-interactively, suitable for
CI / scripting.

Resources are created in the dedicated "route-prism-verify"
namespace and removed on completion (override with --keep-namespace).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			contexts, _, err := verify.LoadContexts(kubeconfigPath)
			if err != nil {
				return err
			}

			opts := verify.Options{
				Namespace:      namespace,
				Timeout:        timeout,
				KeepNamespace:  keepNamespace,
				KubeconfigPath: kubeconfigPath,
				ContextName:    contextName,
				TestImage:      testImage,
				MeshOverride:   preflight.MeshMode(strings.TrimSpace(meshMode)),
				OnExisting:     verify.OnExistingPolicy(strings.TrimSpace(onExisting)),
			}

			// Decide TUI vs plain.
			useTUI := !noTUI && contextName == "" && isatty.IsTerminal(os.Stdout.Fd())

			if !useTUI {
				if contextName == "" {
					// Default to the current context for non-interactive runs.
					for _, c := range contexts {
						if c.IsCurrent {
							contextName = c.Name
							break
						}
					}
					if contextName == "" {
						return fmt.Errorf("no current context in kubeconfig; pass --context")
					}
					opts.ContextName = contextName
				}
				report, cases, err := verify.RunPlain(cmd.Context(), opts)
				return printReport(cmd, report, cases, err)
			}

			model := verify.NewModel(contexts, opts)
			prog := tea.NewProgram(model, tea.WithOutput(os.Stderr))
			if _, err := prog.Run(); err != nil {
				return fmt.Errorf("tui: %w", err)
			}
			return nil
		},
	}

	pf := cmd.Flags()
	pf.StringVar(&kubeconfigPath, "kubeconfig", "", "Explicit kubeconfig path (defaults to the usual precedence)")
	pf.StringVar(&contextName, "context", "", "Run against this kubeconfig context non-interactively")
	pf.StringVar(&namespace, "namespace", verify.VerifyNamespace, "Namespace used for the probe Service/HTTPRoute")
	pf.DurationVar(&timeout, "timeout", 180*time.Second, "Overall probe budget (deploy + accept + traffic)")
	pf.BoolVar(&keepNamespace, "keep-namespace", false, "Do not delete the verify namespace after the probe")
	pf.BoolVar(&noTUI, "no-tui", false, "Always run non-interactively (no TUI)")
	pf.StringVar(&testImage, "test-image", verify.DefaultTestImage, "Operator image used as the test backend (must contain the `test` subcommand)")
	pf.StringVar(&meshMode, "mesh-mode", "", "Override Istio mode detection: ambient | sidecar (default: auto-detect; ignored for non-Istio meshes)")
	pf.StringVar(&onExisting, "on-existing", "",
		"What to do when the namespace already exists with missing mesh labels: "+
			"\"\" abort (default), \"delete\" wipe-and-recreate, \"patch\" add labels in place. "+
			"DANGER: \"patch\" will change dataplane behaviour for every workload in that namespace.")

	return cmd
}

func printReport(cmd *cobra.Command, report preflight.ProbeReport, cases []verify.TrafficCase, err error) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Mesh: %s\n", report.Mesh.Summary())
	_, _ = fmt.Fprintf(out, "Outcome: %s\n", report.Outcome)
	if report.ControllerName != "" {
		_, _ = fmt.Fprintf(out, "Controller: %s\n", report.ControllerName)
	}
	if report.Reason != "" {
		_, _ = fmt.Fprintf(out, "Reason: %s\n", report.Reason)
	}
	if report.Message != "" {
		_, _ = fmt.Fprintf(out, "Message: %s\n", report.Message)
	}
	if len(cases) > 0 {
		_, _ = fmt.Fprintln(out, "\nTraffic cases:")
		for _, tc := range cases {
			marker := "PASS"
			if !tc.OK {
				marker = "FAIL"
			}
			_, _ = fmt.Fprintf(out, "  [%s] %s — expected=%s got=%s", marker, tc.Name, tc.Expected, tc.GotVariant)
			if tc.Err != nil {
				_, _ = fmt.Fprintf(out, "  err=%q", tc.Err.Error())
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	recs := verify.Recommend(report)
	if len(recs) > 0 {
		_, _ = fmt.Fprintln(out, "\nRecommendations:")
		for _, r := range recs {
			_, _ = fmt.Fprintf(out, "  • %s\n", r)
		}
	}
	if err != nil {
		return err
	}
	allPass := report.Outcome == preflight.OutcomeAccepted
	for _, tc := range cases {
		if !tc.OK {
			allPass = false
			break
		}
	}
	if !allPass {
		os.Exit(1)
	}
	return nil
}
