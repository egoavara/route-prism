#!/usr/bin/env bash
# Uninstall operator CRDs from the current kube context.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCALBIN="$(pwd)/bin"
KUSTOMIZE="$LOCALBIN/kustomize"

if [ ! -x "$KUSTOMIZE" ]; then
  bash scripts/install-tools.sh
fi

out="$("$KUSTOMIZE" build config/crd 2>/dev/null || true)"
if [ -n "$out" ]; then
  echo "$out" | kubectl delete --ignore-not-found=true -f -
else
  echo "No CRDs to delete; skipping."
fi
