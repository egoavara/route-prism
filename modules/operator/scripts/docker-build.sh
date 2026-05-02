#!/usr/bin/env bash
# Build manager binary (linux/amd64) + docker image. Used by e2e tests.
set -euo pipefail

cd "$(dirname "$0")/.."

IMG="${IMG:-${1:-controller:latest}}"

# Ensure embed dirs exist; web bundles must already be built into them.
mkdir -p internal/dashboard/dist internal/widget/dist

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager ./cmd
docker build -t "$IMG" -f Dockerfile .
echo "Built image $IMG"
