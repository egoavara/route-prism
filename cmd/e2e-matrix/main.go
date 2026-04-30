/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// e2e-matrix orchestrates the routing regression suite across an N×M
// matrix of (mesh-platform × scenario-group) cells, each on its own
// disposable kind cluster.
//
// For every (platform, group) pair the runner:
//
//  1. spins up a fresh kind cluster via ./hack/kind-up.sh --platform <p>
//     using a dedicated CLUSTER name and KUBECONFIG file
//  2. loads the locally-built manager image into that cluster
//  3. installs CRDs (`make install`) and deploys the controller
//     (`make deploy IMG=...`)
//  4. waits for the controller pod to be Ready
//  5. runs the routing-tagged Ginkgo suite scoped to a single group
//     (`-ginkgo.label-filter='full && <group>'`)
//  6. tears the cluster down (or keeps it on failure when --keep is set)
//
// Output: per-cell streaming logs prefixed with "[<group>/<platform>]" plus
// a final ASCII matrix table of pass/fail/duration.
//
// Defaults:   platforms=cilium,istio,cilium-istio   groups=g1,g3,g4,g7,g8
// Cells are run sequentially by default; bump --parallel for concurrency.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	flagPlatforms   = flag.String("platforms", "cilium,istio,cilium-istio", "comma-separated mesh platforms to test")
	flagGroups      = flag.String("groups", "g1,g3,g4,g7,g8", "comma-separated scenario groups (Ginkgo labels)")
	flagImg         = flag.String("img", "route-prism-e2e:latest", "manager image tag for the matrix")
	flagSkipBuild   = flag.Bool("skip-build", false, "skip 'make docker-build' (assume image already exists)")
	flagKeep        = flag.Bool("keep", false, "leave cluster up on failure for inspection")
	flagParallel    = flag.Int("parallel", 4, "max concurrent cells (0 = all). Each cell uses ~1.5–2 GB RAM and ~2 CPU cores")
	flagTestTimeout = flag.String("test-timeout", "10m", "go test -timeout value for each cell")
)

type GroupResult struct {
	Pass     bool
	Duration time.Duration
	Reason   string
}

// PlatformResult aggregates the per-group results for one platform cluster.
// One disposable kind cluster is provisioned per platform; every scenario
// group runs against that cluster, isolated by namespace (each group's
// BeforeAll provisions e2e-<group> with its own waypoint, workloads, CRs).
type PlatformResult struct {
	Platform     string
	SetupOK      bool
	SetupReason  string                 // populated when cluster setup itself failed
	Groups       map[string]GroupResult // by group name (g1, g3, …)
	TotalRuntime time.Duration
	LogPath      string
}

func main() {
	flag.Parse()

	repo, err := os.Getwd()
	if err != nil {
		fatal("could not determine working dir: %v", err)
	}
	mustBeRepoRoot(repo)

	platforms := splitCSV(*flagPlatforms)
	groups := splitCSV(*flagGroups)
	if len(platforms) == 0 || len(groups) == 0 {
		fatal("at least one platform and one group is required")
	}

	logsDir := filepath.Join(repo, "bin", "e2e-matrix-logs")
	_ = os.MkdirAll(logsDir, 0o755)

	// ── Pre-flight (sequential, one-time) ─────────────────────────
	// Everything here removes a race between parallel cells. Each step
	// is idempotent.
	if !*flagSkipBuild {
		fmt.Printf("→ [pre-flight] building manager image %s\n", *flagImg)
		if err := runStreamed("[build]", "make", "docker-build", "IMG="+*flagImg); err != nil {
			fatal("docker-build failed: %v", err)
		}
	}
	fmt.Println("→ [pre-flight] generating CRD manifests + downloading kustomize/controller-gen")
	if err := runStreamed("[manifests]", "make", "manifests", "kustomize", "controller-gen"); err != nil {
		fatal("make manifests failed: %v", err)
	}
	fmt.Println("→ [pre-flight] prefetching kind / cilium-cli / istioctl binaries")
	if err := runStreamed("[prefetch]", "./hack/kind-up.sh", "--prefetch"); err != nil {
		fatal("prefetch failed: %v", err)
	}
	// Pre-compile the test binary once so cells just exec it (no parallel
	// `go test` cache contention, much faster per-cell startup).
	testBin := filepath.Join(repo, "bin", "e2e-routing.test")
	fmt.Printf("→ [pre-flight] compiling test binary → %s\n", testBin)
	if err := runStreamed("[compile]",
		"go", "test", "-tags", "routing", "-c", "-o", testBin, "./test/e2e/"); err != nil {
		fatal("compile e2e test binary failed: %v", err)
	}

	// One cell == one platform cluster. Within a cell, every scenario
	// group runs sequentially against the same cluster, isolated by the
	// e2e-<group> namespace that BeforeAll provisions inside the routing
	// suite. Parallelism therefore caps at len(platforms).
	parallel := *flagParallel
	if parallel <= 0 || parallel > len(platforms) {
		parallel = len(platforms)
	}
	fmt.Printf("→ [matrix] running %d platform clusters with parallelism=%d (groups within each cluster: %s)\n",
		len(platforms), parallel, strings.Join(groups, ","))

	results := make([]PlatformResult, len(platforms))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, p := range platforms {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = runPlatform(repo, logsDir, testBin, p, groups)
		}()
	}
	wg.Wait()

	printMatrix(results, platforms, groups)

	for _, r := range results {
		if !r.SetupOK {
			os.Exit(1)
		}
		for _, gr := range r.Groups {
			if !gr.Pass {
				os.Exit(1)
			}
		}
	}
}

// runPlatform stands up one disposable kind cluster for a single mesh
// platform, then runs every scenario group sequentially against it. Each
// group lives in its own e2e-<group> namespace (provisioned by the
// routing suite's BeforeAll), so they don't step on each other. Per-group
// results are recorded individually so the final matrix table still shows
// per-(group × platform) granularity.
func runPlatform(repo, logsDir, testBin, platform string, groups []string) PlatformResult {
	cluster := platformClusterName(platform)
	kubeconfig := filepath.Join(repo, "bin", fmt.Sprintf(".kubeconfig-mtx-%s", platform))
	logPath := filepath.Join(logsDir, fmt.Sprintf("platform-%s.log", platform))
	logFile, err := os.Create(logPath)
	if err != nil {
		return PlatformResult{
			Platform:    platform,
			SetupReason: "open log file: " + err.Error(),
			LogPath:     logPath,
		}
	}
	defer logFile.Close()

	header := fmt.Sprintf("=== platform %s · cluster=%s · kubeconfig=%s ===\n", platform, cluster, kubeconfig)
	fmt.Fprint(logFile, header)
	fmt.Print(header)

	stream := io.MultiWriter(logFile, prefixWriter(os.Stdout, "["+platform+"] "))
	errStream := io.MultiWriter(logFile, prefixWriter(os.Stderr, "["+platform+"] "))

	env := append(os.Environ(),
		"CLUSTER="+cluster,
		"KUBECONFIG="+kubeconfig,
	)

	start := time.Now()
	res := PlatformResult{
		Platform: platform,
		Groups:   make(map[string]GroupResult, len(groups)),
		LogPath:  logPath,
	}
	defer func() {
		res.TotalRuntime = time.Since(start)
	}()

	step := func(msg string, args ...any) {
		fmt.Fprintf(stream, "→ "+msg+"\n", args...)
	}

	cleanup := func() {
		step("tearing down kind cluster %s", cluster)
		_ = runWith(env, stream, errStream, repo, "./hack/kind-down.sh")
	}

	// ── Cluster lifecycle: stand up once, share across all groups ──
	step("creating kind cluster (--platform %s)", platform)
	if err := runWith(env, stream, errStream, repo, "./hack/kind-up.sh", "--platform", platform); err != nil {
		res.SetupReason = "kind-up failed: " + err.Error()
		fmt.Fprintf(errStream, "FAIL kind-up: %v\n", err)
		return res
	}

	step("loading manager image %s into kind cluster %s", *flagImg, cluster)
	if err := runWith(env, stream, errStream, repo, "./bin/kind",
		"load", "docker-image", *flagImg, "--name", cluster); err != nil {
		res.SetupReason = "kind load failed"
		fmt.Fprintf(errStream, "FAIL kind load: %v\n", err)
		if !*flagKeep {
			cleanup()
		}
		return res
	}

	step("installing CRDs and deploying controller (inline kustomize)")
	if err := installCRDsInline(env, stream, errStream, repo); err != nil {
		res.SetupReason = "CRD install failed"
		fmt.Fprintf(errStream, "FAIL CRD install: %v\n", err)
		if !*flagKeep {
			cleanup()
		}
		return res
	}
	if err := deployControllerInline(env, stream, errStream, repo, *flagImg); err != nil {
		res.SetupReason = "deploy failed"
		fmt.Fprintf(errStream, "FAIL deploy: %v\n", err)
		if !*flagKeep {
			cleanup()
		}
		return res
	}

	step("waiting for controller-manager Deployment to be Ready")
	if err := runWith(env, stream, errStream, repo,
		"kubectl", "-n", "route-prism-system",
		"rollout", "status", "deployment/route-prism-controller-manager",
		"--timeout=3m"); err != nil {
		res.SetupReason = "controller never became Ready"
		fmt.Fprintf(errStream, "FAIL controller readiness: %v\n", err)
		if !*flagKeep {
			cleanup()
		}
		return res
	}
	res.SetupOK = true

	// ── Run each group as its own test invocation against the same
	//    cluster. Sequential so we can record per-group pass/fail with
	//    no output interleaving. The routing suite's AfterSuite cleans
	//    up suite namespaces after each invocation, but they're scoped
	//    by group anyway (e2e-g1, e2e-g3, …).
	anyFail := false
	for _, g := range groups {
		gStart := time.Now()
		filter := fmt.Sprintf("full && %s", g)
		step("[%s] running test (filter='%s')", g, filter)
		testErr := runWith(env, stream, errStream, repo,
			testBin,
			"-test.v",
			"-test.run", "^TestRouting$",
			"-test.timeout", *flagTestTimeout,
			"-ginkgo.label-filter="+filter,
		)
		gr := GroupResult{
			Pass:     testErr == nil,
			Duration: time.Since(gStart),
		}
		if testErr != nil {
			gr.Reason = testErr.Error()
			anyFail = true
		}
		res.Groups[g] = gr
	}

	if !anyFail || !*flagKeep {
		cleanup()
	} else {
		fmt.Fprintf(stream, "→ KEEPING cluster %s for inspection (KUBECONFIG=%s)\n", cluster, kubeconfig)
	}
	return res
}

// installCRDsInline renders config/crd with kustomize and pipes it to
// kubectl apply. Replaces `make install`, which would re-run controller-gen
// (mutating config/crd/bases/) and is therefore unsafe under parallelism.
// Pre-flight already ran `make manifests` once.
func installCRDsInline(env []string, stdout, stderr io.Writer, repo string) error {
	kustomize := filepath.Join(repo, "bin", "kustomize")
	build := exec.Command(kustomize, "build", "config/crd")
	build.Env = env
	build.Dir = repo
	build.Stderr = stderr
	out, err := build.Output()
	if err != nil {
		return fmt.Errorf("kustomize build config/crd: %w", err)
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "→ no CRDs to install (config/crd is empty)")
		return nil
	}
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Env = env
	apply.Dir = repo
	apply.Stdin = strings.NewReader(string(out))
	apply.Stdout = stdout
	apply.Stderr = stderr
	return apply.Run()
}

// deployControllerInline renders config/default with kustomize, replaces
// the placeholder image (controller:latest) with the matrix image, forces
// imagePullPolicy=IfNotPresent on that container, and pipes the result to
// kubectl apply. Crucially this does NOT touch config/manager/
// kustomization.yaml on disk, so concurrent Tilt sessions keep working
// against the dev cluster.
//
// Why force IfNotPresent: the image is delivered to nodes via `kind load
// docker-image`, not pushed to a registry. Kubernetes defaults to Always
// for :latest tags, which would re-pull from a registry that has no copy.
// Forcing IfNotPresent makes the kubelet use the locally-loaded image.
func deployControllerInline(env []string, stdout, stderr io.Writer, repo, img string) error {
	kustomize := filepath.Join(repo, "bin", "kustomize")
	build := exec.Command(kustomize, "build", "config/default")
	build.Env = env
	build.Dir = repo
	build.Stderr = stderr
	out, err := build.Output()
	if err != nil {
		return fmt.Errorf("kustomize build: %w", err)
	}
	manifest := strings.ReplaceAll(string(out), "controller:latest", img)
	manifest = injectIfNotPresent(manifest, img)

	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Env = env
	apply.Dir = repo
	apply.Stdin = strings.NewReader(manifest)
	apply.Stdout = stdout
	apply.Stderr = stderr
	return apply.Run()
}

// injectIfNotPresent inserts an `imagePullPolicy: IfNotPresent` line right
// after every `image: <img>` line that targets the matrix image, copying
// the leading indentation so the YAML stays valid. We don't touch lines
// that already declare imagePullPolicy.
func injectIfNotPresent(manifest, img string) string {
	needle := "image: " + img
	lines := strings.Split(manifest, "\n")
	out := make([]string, 0, len(lines)+4)
	for i, ln := range lines {
		out = append(out, ln)
		if !strings.Contains(ln, needle) {
			continue
		}
		// Skip if a sibling imagePullPolicy already exists on the next line.
		if i+1 < len(lines) && strings.Contains(lines[i+1], "imagePullPolicy") {
			continue
		}
		// Copy the leading whitespace from the image line so the new line
		// stays at the same indent level (sibling YAML key).
		ws := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		out = append(out, ws+"imagePullPolicy: IfNotPresent")
	}
	return strings.Join(out, "\n")
}

// ── helpers ────────────────────────────────────────────────────────

func runWith(env []string, stdout, stderr io.Writer, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// runStreamed runs a pre-flight command with prefixed stdout/stderr.
func runStreamed(prefix string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = prefixWriter(os.Stdout, prefix+" ")
	cmd.Stderr = prefixWriter(os.Stderr, prefix+" ")
	return cmd.Run()
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// platformClusterName produces a kind cluster name like "rpmtx-cilium",
// "rpmtx-istio", "rpmtx-cilium-istio". Distinct from the dev cluster
// ("route-prism") so a matrix run can coexist with Tilt.
func platformClusterName(platform string) string {
	return "rpmtx-" + platform
}

func mustBeRepoRoot(dir string) {
	for _, sentinel := range []string{"hack/kind-up.sh", "Makefile", "test/e2e"} {
		if _, err := os.Stat(filepath.Join(dir, sentinel)); err != nil {
			fatal("must be run from repo root (missing %s)", sentinel)
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(2)
}

// prefixWriter wraps w so every line written gets `prefix` prepended.
func prefixWriter(w io.Writer, prefix string) io.Writer {
	return &prefixedWriter{w: w, prefix: []byte(prefix), atLineStart: true}
}

type prefixedWriter struct {
	w           io.Writer
	prefix      []byte
	mu          sync.Mutex
	atLineStart bool
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	written := 0
	for _, c := range b {
		if p.atLineStart {
			if _, err := p.w.Write(p.prefix); err != nil {
				return written, err
			}
			p.atLineStart = false
		}
		if _, err := p.w.Write([]byte{c}); err != nil {
			return written, err
		}
		written++
		if c == '\n' {
			p.atLineStart = true
		}
	}
	return written, nil
}

// printMatrix emits a fixed-width matrix table to stdout.
//   rows    = scenario groups (g1, g3, …) — one extra row "SETUP" surfaces
//             cluster-lifecycle failures that prevented any group from running.
//   columns = mesh platforms (cilium, istio, cilium-istio)
// Each body cell shows PASS/FAIL plus duration.
func printMatrix(results []PlatformResult, platforms, groups []string) {
	idx := make(map[string]PlatformResult, len(results))
	for _, r := range results {
		idx[r.Platform] = r
	}

	const colW = 16
	sep := strings.Repeat("─", 8+(colW+3)*len(platforms))

	fmt.Println()
	fmt.Println(sep)
	fmt.Println("MATRIX RESULTS")
	fmt.Println(sep)

	// header
	var hdr strings.Builder
	hdr.WriteString(fmt.Sprintf("%-8s", ""))
	for _, p := range platforms {
		hdr.WriteString(fmt.Sprintf(" │ %-*s", colW, p))
	}
	fmt.Println(hdr.String())
	fmt.Println(sep)

	pass, fail := 0, 0

	// SETUP row — shows cluster lifecycle status (kind-up + image load +
	// CRD install + deploy + controller readiness). When this fails for a
	// platform, every group cell below is skipped (rendered "—").
	var setupRow strings.Builder
	setupRow.WriteString(fmt.Sprintf("%-8s", "SETUP"))
	for _, p := range platforms {
		r := idx[p]
		cell := "—"
		if r.Platform != "" {
			if r.SetupOK {
				cell = fmt.Sprintf("\033[32mPASS\033[0m %s", shortDur(r.TotalRuntime))
			} else {
				cell = fmt.Sprintf("\033[31mFAIL\033[0m %s", shortDur(r.TotalRuntime))
			}
		}
		setupRow.WriteString(fmt.Sprintf(" │ %-*s", colW+9 /* ANSI overhead */, cell))
	}
	fmt.Println(setupRow.String())
	fmt.Println(sep)

	for _, g := range groups {
		var row strings.Builder
		row.WriteString(fmt.Sprintf("%-8s", strings.ToUpper(g)))
		for _, p := range platforms {
			r := idx[p]
			cell := "—"
			if r.SetupOK {
				gr, ok := r.Groups[g]
				if ok {
					if gr.Pass {
						cell = fmt.Sprintf("\033[32mPASS\033[0m %s", shortDur(gr.Duration))
						pass++
					} else {
						cell = fmt.Sprintf("\033[31mFAIL\033[0m %s", shortDur(gr.Duration))
						fail++
					}
				}
			} else if r.Platform != "" {
				cell = "\033[33mSKIP\033[0m (setup)"
			}
			row.WriteString(fmt.Sprintf(" │ %-*s", colW+9, cell))
		}
		fmt.Println(row.String())
	}
	fmt.Println(sep)
	fmt.Printf("Total: %d passed, %d failed (per-platform logs in bin/e2e-matrix-logs/platform-<p>.log)\n", pass, fail)
	fmt.Println(sep)
}

func shortDur(d time.Duration) string {
	if d == 0 {
		return ""
	}
	if d >= time.Minute {
		return fmt.Sprintf("(%dm%02ds)", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("(%ds)", int(d.Seconds()))
}
