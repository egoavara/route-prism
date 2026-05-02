#!/usr/bin/env bash
# Launch Tilt against the route-prism kind cluster.
#
# Prerequisites: run `./hack/kind-up.sh` first to create the cluster + Cilium.
# This wrapper only sets the project-local KUBECONFIG and PATH, then execs Tilt.
#
# Usage: ./hack/tilt-up.sh [extra tilt args...]
set -euo pipefail

CLUSTER="${CLUSTER:-route-prism}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
KIND="${REPO_ROOT}/bin/kind"
export KUBECONFIG="${REPO_ROOT}/bin/.kubeconfig"
export PATH="${REPO_ROOT}/bin:${PATH}"

if [[ ! -x "${KIND}" ]]; then
    echo "ERROR: ${KIND} not found. Run ./hack/kind-up.sh first." >&2
    exit 1
fi

if ! "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    cat >&2 <<EOF
ERROR: kind cluster '${CLUSTER}' does not exist.

Create it first:
    ./hack/kind-up.sh
EOF
    exit 1
fi

# Refresh project-local kubeconfig so it points at the live cluster.
"${KIND}" export kubeconfig --name "${CLUSTER}" >/dev/null
chmod 600 "${KUBECONFIG}" 2>/dev/null || true

if ! kubectl config current-context | grep -qx "kind-${CLUSTER}"; then
    echo "ERROR: kubectl current-context is not kind-${CLUSTER}." >&2
    kubectl config get-contexts >&2
    exit 1
fi

if ! command -v tilt >/dev/null 2>&1; then
    echo "ERROR: tilt not on PATH. Install: https://docs.tilt.dev/install.html" >&2
    exit 1
fi

echo "✓ KUBECONFIG=${KUBECONFIG} (context: kind-${CLUSTER})"
exec tilt up "$@"
