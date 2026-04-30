#!/usr/bin/env bash
# Tear down the kind cluster created by ./hack/kind-up.sh.
# Leaves ./bin/kind, ./bin/cilium binaries in place so the next kind-up.sh
# is fast.
#
# Usage: ./hack/kind-down.sh
set -euo pipefail

CLUSTER="${CLUSTER:-route-prism}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
KIND="${REPO_ROOT}/bin/kind"
KUBECONFIG_FILE="${KUBECONFIG:-${REPO_ROOT}/bin/.kubeconfig}"

if [[ ! -x "${KIND}" ]]; then
    echo "kind binary not found at ${KIND}; nothing to do." >&2
    exit 0
fi

if "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "→ deleting kind cluster '${CLUSTER}' ..."
    "${KIND}" delete cluster --name "${CLUSTER}"
else
    echo "✓ kind cluster '${CLUSTER}' is not present"
fi

# Wipe project-local kubeconfig so the next session starts clean.
if [[ -f "${KUBECONFIG_FILE}" ]]; then
    rm -f "${KUBECONFIG_FILE}"
    echo "✓ removed ${KUBECONFIG_FILE}"
fi
