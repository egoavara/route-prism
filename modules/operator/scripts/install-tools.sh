#!/usr/bin/env bash
# Install operator build/test tools into ./bin (per kubebuilder convention).
# Each tool is pinned by version and installed only if the versioned binary is missing.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
mkdir -p "$LOCALBIN"

# Ensure embed targets exist so `//go:embed all:dist` doesn't error on a
# fresh checkout where web bundles haven't been built yet.
mkdir -p internal/dashboard/dist internal/widget/dist

CONTROLLER_TOOLS_VERSION="${CONTROLLER_TOOLS_VERSION:-v0.20.1}"
KUSTOMIZE_VERSION="${KUSTOMIZE_VERSION:-v5.8.1}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.8.0}"

# Derive ENVTEST_VERSION (release branch) from go.mod controller-runtime pin.
gomodver() {
  go mod download "$1" >/dev/null 2>&1 || true
  go list -m -f '{{.Version}}' "$1" 2>/dev/null
}

CR_VER="$(gomodver sigs.k8s.io/controller-runtime || true)"
if [ -n "$CR_VER" ]; then
  ENVTEST_VERSION="release-$(echo "$CR_VER" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/\1.\2/')"
else
  ENVTEST_VERSION="${ENVTEST_VERSION:?Set ENVTEST_VERSION manually}"
fi

install_tool() {
  local name="$1" pkg="$2" version="$3"
  local target="$LOCALBIN/$name"
  local versioned="$target-$version"
  if [ -L "$target" ] && [ "$(readlink -- "$target")" = "$versioned" ] && [ -x "$versioned" ]; then
    return 0
  fi
  echo "Installing $name@$version"
  GOBIN="$LOCALBIN" go install "$pkg@$version"
  mv -f "$target" "$versioned"
  ln -sfn "$versioned" "$target"
}

install_tool controller-gen sigs.k8s.io/controller-tools/cmd/controller-gen "$CONTROLLER_TOOLS_VERSION"
install_tool kustomize sigs.k8s.io/kustomize/kustomize/v5 "$KUSTOMIZE_VERSION"
install_tool setup-envtest sigs.k8s.io/controller-runtime/tools/setup-envtest "$ENVTEST_VERSION"
install_tool golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint "$GOLANGCI_LINT_VERSION"

# Rebuild golangci-lint with custom plugins if .custom-gcl.yml is present.
if [ -f .custom-gcl.yml ]; then
  echo "Building custom golangci-lint with plugins..."
  "$LOCALBIN/golangci-lint" custom --destination "$LOCALBIN" --name golangci-lint-custom
  mv -f "$LOCALBIN/golangci-lint-custom" "$LOCALBIN/golangci-lint"
fi
