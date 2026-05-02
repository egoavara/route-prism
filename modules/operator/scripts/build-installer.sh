#!/usr/bin/env bash
# Build the consolidated install.yaml (CRDs + RBAC + Deployment) under ./dist.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
KUSTOMIZE="$LOCALBIN/kustomize"
IMG="${IMG:-controller:latest}"

mkdir -p dist
(cd config/manager && "$KUSTOMIZE" edit set image controller="$IMG")
"$KUSTOMIZE" build config/default > dist/install.yaml
echo "Wrote dist/install.yaml (image=$IMG)"
