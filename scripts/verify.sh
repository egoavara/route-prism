#!/usr/bin/env bash
#
# verify.sh — download the latest route-prism operator binary and run
# `route-prism verify` against your current kubeconfig.
#
#   curl -sSL https://raw.githubusercontent.com/egoavara/route-prism/main/scripts/verify.sh | bash
#   curl -sSL https://raw.githubusercontent.com/egoavara/route-prism/main/scripts/verify.sh | bash -s -- --context my-cluster
#
# Environment overrides:
#   ROUTE_PRISM_VERSION   pin a release tag (default: latest)
#   ROUTE_PRISM_BIN_DIR   cache directory (default: $HOME/.cache/route-prism)
set -euo pipefail

REPO="egoavara/route-prism"
VERSION="${ROUTE_PRISM_VERSION:-}"
BIN_DIR="${ROUTE_PRISM_BIN_DIR:-$HOME/.cache/route-prism}"

# --- detect OS/arch ---------------------------------------------------------
uname_s="$(uname -s)"
uname_m="$(uname -m)"
case "$uname_s" in
  Linux)   os="linux" ;;
  Darwin)  os="linux" ;;  # no native macOS build; require Linux container or Rosetta. Fall through gracefully.
  *) echo "verify.sh: unsupported OS '$uname_s' — only linux is shipped today" >&2; exit 1 ;;
esac
case "$uname_m" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "verify.sh: unsupported arch '$uname_m'" >&2; exit 1 ;;
esac
if [[ "$uname_s" == "Darwin" ]]; then
  echo "verify.sh: macOS is not directly supported yet — falling back to the linux/${arch} build, which only runs under Rosetta or a Linux VM." >&2
fi

# --- resolve version --------------------------------------------------------
if [[ -z "$VERSION" ]]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -nE 's/^[[:space:]]*"tag_name":[[:space:]]*"([^"]+)".*/\1/p' | head -n1)"
  if [[ -z "$VERSION" ]]; then
    echo "verify.sh: failed to resolve latest release tag" >&2; exit 1
  fi
fi
ver_no_v="${VERSION#v}"

# --- fetch binary -----------------------------------------------------------
asset="route-prism-operator_${ver_no_v}_${os}_${arch}"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
mkdir -p "$BIN_DIR"
bin="$BIN_DIR/${asset}"

if [[ ! -x "$bin" ]]; then
  echo "verify.sh: downloading $asset ($VERSION) ..." >&2
  curl -fsSL "$url" -o "$bin"
  chmod +x "$bin"
fi

# --- run --------------------------------------------------------------------
exec "$bin" verify "$@"
