#!/usr/bin/env bash
# Deploy controller-manager via kustomize/kubectl.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
KUSTOMIZE="$LOCALBIN/kustomize"
IMG="${IMG:-${1:-controller:latest}}"

if [ ! -x "$KUSTOMIZE" ]; then
  bash scripts/install-tools.sh
fi

(cd config/manager && "$KUSTOMIZE" edit set image controller="$IMG")
"$KUSTOMIZE" build config/default | kubectl apply -f -
