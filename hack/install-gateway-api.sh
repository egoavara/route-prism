#!/usr/bin/env bash
# Install Gateway API standard channel CRDs (v1.2.1).
# Usage: ./hack/install-gateway-api.sh [version]
set -euo pipefail

VERSION="${1:-v1.2.1}"

kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${VERSION}/standard-install.yaml"
echo "Gateway API ${VERSION} CRDs installed."
