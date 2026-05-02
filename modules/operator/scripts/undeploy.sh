#!/usr/bin/env bash
# Undeploy controller-manager.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
KUSTOMIZE="$LOCALBIN/kustomize"

if [ ! -x "$KUSTOMIZE" ]; then
  bash scripts/install-tools.sh
fi

"$KUSTOMIZE" build config/default | kubectl delete --ignore-not-found=true -f -
