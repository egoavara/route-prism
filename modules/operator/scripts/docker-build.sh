#!/usr/bin/env bash
# Build manager binary (linux/amd64) + docker image. Used by e2e tests.
set -euo pipefail

cd "$(dirname "$0")/.."

IMG="${IMG:-${1:-controller:latest}}"

# Ensure embed dirs exist; web bundles must already be built into them.
mkdir -p internal/dashboard/dist internal/widget/dist

echo "[docker-build] pwd=$(pwd) IMG=$IMG"
ls -la internal/dashboard/dist/ internal/widget/dist/ 2>&1 | head -10

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager ./cmd
ls -la bin/manager

DOCKER_BUILDKIT=0 docker build -t "$IMG" -f Dockerfile .
echo "[docker-build] images list:"
docker images "$IMG"
docker image inspect "$IMG" --format 'OK: id={{.Id}} tags={{.RepoTags}}'
