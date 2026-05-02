/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MeshKind enumerates service mesh implementations route-prism is aware of.
type MeshKind string

const (
	MeshUnknown MeshKind = ""
	MeshIstio   MeshKind = "istio"
	MeshCilium  MeshKind = "cilium"
)

// MeshMode is the dataplane flavour, relevant to namespaces / waypoint
// requirements. Only meaningful for Istio today.
type MeshMode string

const (
	MeshModeUnknown MeshMode = ""
	MeshModeSidecar MeshMode = "sidecar" // istio-injection=enabled namespace label
	MeshModeAmbient MeshMode = "ambient" // istio.io/dataplane-mode=ambient + waypoint
	MeshModeNative  MeshMode = "native"  // mesh handles routing without per-namespace opt-in (Cilium)
)

// MeshInfo carries best-effort identification of the surrounding service
// mesh. All fields are optional — population depends on what was readable
// in istio-system / kube-system.
type MeshInfo struct {
	Kind    MeshKind
	Mode    MeshMode
	Version Version // zero value if unparseable
	// RawImage is the container image string the version was parsed from,
	// kept around for surfacing in event messages.
	RawImage string
}

// Summary produces a short human-readable form for event messages.
func (m MeshInfo) Summary() string {
	switch m.Kind {
	case MeshUnknown:
		return "unknown (neither istiod nor cilium-operator found)"
	default:
		base := string(m.Kind)
		if !m.Version.IsZero() {
			base = fmt.Sprintf("%s %s", m.Kind, m.Version)
		} else if m.RawImage != "" {
			base = fmt.Sprintf("%s (image=%q)", m.Kind, m.RawImage)
		}
		if m.Mode != "" && m.Mode != MeshModeNative {
			base += " (" + string(m.Mode) + ")"
		}
		return base
	}
}

// NamespaceLabels returns the labels that must be set on a namespace for
// the mesh to actually capture traffic from Pods running there. Returns
// nil for meshes (Cilium) that route traffic without per-namespace opt-in.
func (m MeshInfo) NamespaceLabels() map[string]string {
	switch m.Kind {
	case MeshIstio:
		switch m.Mode {
		case MeshModeAmbient:
			return map[string]string{"istio.io/dataplane-mode": "ambient"}
		case MeshModeSidecar:
			return map[string]string{"istio-injection": "enabled"}
		}
	}
	return nil
}

// RequiresWaypoint indicates whether GAMMA L7 routing on this mesh needs
// a waypoint Gateway in the target namespace plus an
// `istio.io/use-waypoint` annotation on the parent Service.
func (m MeshInfo) RequiresWaypoint() bool {
	return m.Kind == MeshIstio && m.Mode == MeshModeAmbient
}

// Experimental reports whether the detected mesh version is in the
// "may work but not officially stable" band. Surfaced as a softer warning.
func (m MeshInfo) Experimental() bool {
	if m.Version.IsZero() {
		return false
	}
	switch m.Kind {
	case MeshIstio:
		// 1.20–1.21: Gateway API GA but GAMMA still alpha/experimental.
		// 1.22+: GAMMA stable per Istio 1.22 release announcement.
		return m.Version.AtLeast(Version{1, 20, 0}) && m.Version.LessThan(Version{1, 22, 0})
	}
	return false
}

// KnownIssue returns a non-empty message when the detected version is in a
// matrix of versions known to lack or have buggy GAMMA support. Empty
// string means "no version-specific concern".
//
// References used to populate this matrix:
//   - Istio 1.22 announcement: "Gateway API now Stable for service mesh"
//     (https://istio.io/latest/news/releases/1.22.x/announcing-1.22/)
//   - Cilium 1.16 release: introduced GAMMA support
//     (https://isovalent.com/blog/post/cilium-1-16/)
func (m MeshInfo) KnownIssue() string {
	if m.Version.IsZero() {
		return ""
	}
	switch m.Kind {
	case MeshIstio:
		switch {
		case m.Version.LessThan(Version{1, 20, 0}):
			return fmt.Sprintf("Istio %s does not support Gateway API GAMMA (HTTPRoute mesh binding); minimum recommended is 1.22+ (stable).", m.Version)
		case m.Version.LessThan(Version{1, 22, 0}):
			return fmt.Sprintf("Istio %s has experimental/alpha GAMMA support only; 1.22+ is required for stable HTTPRoute mesh binding.", m.Version)
		}
	case MeshCilium:
		if m.Version.LessThan(Version{1, 16, 0}) {
			return fmt.Sprintf("Cilium %s does not support Gateway API GAMMA; minimum is 1.16.", m.Version)
		}
	}
	return ""
}

// DetectMesh probes well-known namespaces for the supported mesh
// implementations and parses their version. Errors are swallowed: any
// failure results in MeshUnknown, since detection is informational.
func DetectMesh(ctx context.Context, c client.Client) MeshInfo {
	// Istio: istio-system/istiod Deployment.
	if info, ok := readDeploymentImage(ctx, c, "istio-system", "istiod"); ok {
		mode := MeshModeSidecar
		// Ambient mode adds a ztunnel DaemonSet in istio-system. Its
		// presence is the canonical signal that the install is ambient.
		if hasDaemonSet(ctx, c, "istio-system", "ztunnel") {
			mode = MeshModeAmbient
		}
		return MeshInfo{
			Kind:     MeshIstio,
			Mode:     mode,
			Version:  parseVersionFromImage(info, []string{"istiod:", "pilot:", "proxyv2:"}),
			RawImage: info,
		}
	}
	// Cilium: kube-system/cilium-operator Deployment (also tries the generic
	// cilium-operator-generic name for chart variants that pin a single arch).
	for _, name := range []string{"cilium-operator", "cilium-operator-generic"} {
		if info, ok := readDeploymentImage(ctx, c, "kube-system", name); ok {
			return MeshInfo{
				Kind:     MeshCilium,
				Mode:     MeshModeNative,
				Version:  parseVersionFromImage(info, []string{"cilium-operator-generic:", "cilium-operator:", "operator-generic:", "operator:", "cilium:"}),
				RawImage: info,
			}
		}
	}
	return MeshInfo{Kind: MeshUnknown}
}

func hasDaemonSet(ctx context.Context, c client.Client, ns, name string) bool {
	var ds appsv1.DaemonSet
	return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ds) == nil
}

func readDeploymentImage(ctx context.Context, c client.Client, ns, name string) (string, bool) {
	var dep appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &dep); err != nil {
		return "", false
	}
	for _, ct := range dep.Spec.Template.Spec.Containers {
		// Pick the first container that looks like the workload itself.
		// In practice both istiod and cilium-operator have a single
		// primary container.
		return ct.Image, true
	}
	return "", false
}

// parseVersionFromImage extracts a semver-ish triple from a container
// image string. Tries the provided component-name prefixes first
// ("istiod:1.21.3"), then falls back to grabbing the substring after the
// last ':' (covers "image@sha256:..." poorly, hence prefix-first).
func parseVersionFromImage(image string, prefixes []string) Version {
	for _, p := range prefixes {
		if _, after, ok := strings.Cut(image, p); ok {
			return parseVersion(after)
		}
	}
	if idx := strings.LastIndex(image, ":"); idx >= 0 && idx < len(image)-1 {
		tag := image[idx+1:]
		// Skip digest-form tags ("sha256-..." after '@' is the canonical
		// form, but pinned charts sometimes use ":sha256-..." too).
		if strings.HasPrefix(tag, "sha256") {
			return Version{}
		}
		return parseVersion(tag)
	}
	return Version{}
}

// Version is a permissive major.minor.patch holder. Pre-release / build
// suffixes (e.g. "1.22.0-rc.1") are accepted by stripping at the first
// non-numeric character of each component.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) IsZero() bool { return v.Major == 0 && v.Minor == 0 && v.Patch == 0 }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v Version) AtLeast(o Version) bool { return !v.LessThan(o) }

func (v Version) LessThan(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

func parseVersion(s string) Version {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return Version{}
	}
	major, ok := atoiPrefix(parts[0])
	if !ok {
		return Version{}
	}
	minor, ok := atoiPrefix(parts[1])
	if !ok {
		return Version{}
	}
	patch := 0
	if len(parts) == 3 {
		patch, _ = atoiPrefix(parts[2])
	}
	return Version{Major: major, Minor: minor, Patch: patch}
}

// atoiPrefix parses the leading numeric run of s. Returns (n, true) if at
// least one digit was consumed.
func atoiPrefix(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
