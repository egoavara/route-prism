#!/usr/bin/env bash
# Run unit tests with envtest assets staged into ./bin.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
ENVTEST="$LOCALBIN/setup-envtest"

# Derive ENVTEST_K8S_VERSION (e.g. 1.34) from k8s.io/api pin in go.mod when not explicitly set.
if [ -z "${ENVTEST_K8S_VERSION:-}" ]; then
  K8S_VER="$(go list -m -f '{{.Version}}' k8s.io/api 2>/dev/null)"
  if [ -n "$K8S_VER" ]; then
    ENVTEST_K8S_VERSION="1.$(echo "$K8S_VER" | sed -E 's/^v?[0-9]+\.([0-9]+).*/\1/')"
  else
    echo "Set ENVTEST_K8S_VERSION manually (could not infer from go.mod)" >&2
    exit 1
  fi
fi

echo "Setting up envtest binaries for Kubernetes $ENVTEST_K8S_VERSION..."
KUBEBUILDER_ASSETS="$("$ENVTEST" use "$ENVTEST_K8S_VERSION" --bin-dir "$LOCALBIN" -p path)"
export KUBEBUILDER_ASSETS

# Fetch Gateway API CRDs (some controller tests reference them).
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-$(go list -m -f '{{.Version}}' sigs.k8s.io/gateway-api 2>/dev/null)}"
if [ -n "$GATEWAY_API_VERSION" ]; then
  GW_DIR="$LOCALBIN/gateway-api"
  GW_FILE="$GW_DIR/standard-install.yaml"
  if [ ! -f "$GW_FILE" ]; then
    mkdir -p "$GW_DIR"
    echo "Fetching Gateway API CRDs $GATEWAY_API_VERSION..."
    curl -fsSLo "$GW_FILE" "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml"
  fi
fi

go test "$@" $(go list ./... | grep -v /e2e) -coverprofile cover.out
