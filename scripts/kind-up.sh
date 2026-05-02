#!/usr/bin/env bash
# Create a kind cluster wired for Gateway API (GAMMA), with the chosen
# service-mesh platform: Cilium or Istio (ambient).
#
# Usage:
#   ./scripts/kind-up.sh                            # defaults to cilium
#   ./scripts/kind-up.sh --platform cilium          # Cilium provides CNI + GAMMA
#   ./scripts/kind-up.sh --platform istio           # kindnet CNI + Istio ambient GAMMA
#   ./scripts/kind-up.sh --platform cilium-istio    # Cilium CNI (chained) + Istio ambient
#                                                # (Cilium installed with cni.exclusive=false
#                                                # and gatewayAPI off so Istio is the sole
#                                                # GAMMA enforcer)
#   PLATFORM=cilium-istio ./scripts/kind-up.sh
#
# Idempotent: if the cluster already exists with the chosen mesh running,
# this exits successfully without touching anything. If the cluster exists
# but the mesh is missing OR a *different* mesh is installed, it errors out
# — recreate via ./scripts/kind-down.sh && ./scripts/kind-up.sh --platform <p>.
set -euo pipefail

PLATFORM="${PLATFORM:-cilium}"
PREFETCH=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --platform)
            PLATFORM="$2"; shift 2 ;;
        --platform=*)
            PLATFORM="${1#--platform=}"; shift ;;
        --prefetch)
            # Download every tool binary the script can possibly need
            # (kind, cilium-cli, istioctl) and exit. Used by parallel
            # orchestrators so downstream cells don't race on first-time
            # downloads.
            PREFETCH=1; shift ;;
        -h|--help)
            sed -n '2,12p' "$0"; exit 0 ;;
        *)
            echo "ERROR: unknown argument: $1" >&2; exit 2 ;;
    esac
done
case "${PLATFORM}" in
    cilium|istio|cilium-istio) ;;
    *) echo "ERROR: --platform must be 'cilium', 'istio', or 'cilium-istio' (got: ${PLATFORM})" >&2; exit 2 ;;
esac

CLUSTER="${CLUSTER:-route-prism}"
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
CILIUM_CLI_VERSION="${CILIUM_CLI_VERSION:-v0.16.24}"
CILIUM_VERSION="${CILIUM_VERSION:-1.19.3}"   # GAMMA support: stable in 1.17+; 1.18.1+ recommended
ISTIO_VERSION="${ISTIO_VERSION:-1.29.2}"     # GAMMA stable since 1.22; ambient profile available
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.4.1}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
mkdir -p bin
KIND="${REPO_ROOT}/bin/kind"
CILIUM="${REPO_ROOT}/bin/cilium"
ISTIOCTL="${REPO_ROOT}/bin/istioctl"
export KUBECONFIG="${KUBECONFIG:-${REPO_ROOT}/bin/.kubeconfig}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH_RAW=$(uname -m)
case "${ARCH_RAW}" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "ERROR: unsupported arch ${ARCH_RAW}" >&2; exit 1 ;;
esac

# Tool-binary download helpers. Defined here (above the procedural section)
# so --prefetch can call them without running any of the cluster lifecycle.
download_kind() {
    if [[ -x "${KIND}" ]] && "${KIND}" version 2>/dev/null | grep -q "${KIND_VERSION}"; then
        return 0
    fi
    echo "→ downloading kind ${KIND_VERSION} → ${KIND}"
    curl -fsSL -o "${KIND}" "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-${OS}-${ARCH}"
    chmod +x "${KIND}"
}

download_cilium_cli() {
    if [[ -x "${CILIUM}" ]] && "${CILIUM}" version --client 2>/dev/null | grep -q "${CILIUM_CLI_VERSION#v}"; then
        return 0
    fi
    echo "→ downloading cilium CLI ${CILIUM_CLI_VERSION} → ${CILIUM}"
    local TMP; TMP=$(mktemp -d)
    curl -fsSL -o "${TMP}/cilium.tar.gz" \
        "https://github.com/cilium/cilium-cli/releases/download/${CILIUM_CLI_VERSION}/cilium-${OS}-${ARCH}.tar.gz"
    tar -xzf "${TMP}/cilium.tar.gz" -C "${TMP}"
    mv "${TMP}/cilium" "${CILIUM}"
    chmod +x "${CILIUM}"
    rm -rf "${TMP}"
}

download_istioctl() {
    if [[ -x "${ISTIOCTL}" ]] && "${ISTIOCTL}" version --remote=false 2>/dev/null | grep -qx "${ISTIO_VERSION}"; then
        return 0
    fi
    echo "→ downloading istioctl ${ISTIO_VERSION} → ${ISTIOCTL}"
    local TMP; TMP=$(mktemp -d)
    local ISTIO_OS="${OS}"
    [[ "${ISTIO_OS}" == "darwin" ]] && ISTIO_OS="osx"
    curl -fsSL -o "${TMP}/istioctl.tar.gz" \
        "https://github.com/istio/istio/releases/download/${ISTIO_VERSION}/istioctl-${ISTIO_VERSION}-${ISTIO_OS}-${ARCH}.tar.gz"
    tar -xzf "${TMP}/istioctl.tar.gz" -C "${TMP}"
    mv "${TMP}/istioctl" "${ISTIOCTL}"
    chmod +x "${ISTIOCTL}"
    rm -rf "${TMP}"
}

# --prefetch: download every binary and exit. Cluster lifecycle skipped.
if [[ "${PREFETCH}" == "1" ]]; then
    download_kind
    download_cilium_cli
    download_istioctl
    echo "✓ tool binaries ready (kind, cilium, istioctl in ${REPO_ROOT}/bin/)"
    exit 0
fi

# 1) docker reachable?
if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: docker not on PATH. Install: https://docs.docker.com/get-docker/" >&2
    exit 1
fi
if ! docker info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
ERROR: docker is on PATH but the daemon is not reachable.
  - Make sure Docker Desktop is running.
  - On WSL2: Docker Desktop → Settings → Resources → WSL integration,
    enable integration for this distro, then restart Docker Desktop.
EOF
    exit 1
fi

# 2) kubectl present?
if ! command -v kubectl >/dev/null 2>&1; then
    echo "ERROR: kubectl not on PATH." >&2
    exit 1
fi

# 3) kind binary
download_kind

# 4) kind cluster (config differs per platform).
#    cilium       → kindnet off, kube-proxy off (Cilium handles both)
#    istio        → kindnet on,  kube-proxy on  (Istio sits on top)
#    cilium-istio → kindnet off, kube-proxy ON  (Cilium = CNI only;
#                   kube-proxy required for Istio ambient's iptables
#                   redirection to fire — otherwise Cilium socket-LB
#                   short-circuits Service→Pod translation and bypasses
#                   ztunnel/waypoint).
KIND_CONFIG_BASENAME=kind-config-${PLATFORM}.yaml
KIND_CONFIG="${REPO_ROOT}/scripts/${KIND_CONFIG_BASENAME}"
if [[ ! -f "${KIND_CONFIG}" ]]; then
    echo "ERROR: missing ${KIND_CONFIG}" >&2; exit 1
fi
if ! "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "→ creating kind cluster '${CLUSTER}' (${PLATFORM} config: ${KIND_CONFIG##*/}) ..."
    # The shared config files intentionally OMIT `name:` so a single config
    # serves both the dev cluster and the matrix runner. The cluster name
    # comes from $CLUSTER (default: route-prism). When the caller exports
    # CLUSTER=rpmtx-... this --name override gives them a fresh disposable
    # cluster that won't collide with the dev one.
    "${KIND}" create cluster --name "${CLUSTER}" --config "${KIND_CONFIG}"
    NEW_CLUSTER=1
else
    echo "✓ kind cluster '${CLUSTER}' already exists"
    NEW_CLUSTER=0
fi

# 5) Force-(re)export kind kubeconfig into the project-local file.
"${KIND}" export kubeconfig --name "${CLUSTER}" >/dev/null
chmod 600 "${KUBECONFIG}" 2>/dev/null || true

if ! kubectl config current-context | grep -qx "kind-${CLUSTER}"; then
    echo "ERROR: kubectl current-context is not kind-${CLUSTER}. Aborting." >&2
    kubectl config get-contexts >&2
    exit 1
fi
echo "✓ KUBECONFIG=${KUBECONFIG} (current-context: kind-${CLUSTER})"

# 6) Detect if the cluster was created with a different layout than requested.
HAS_CILIUM=0; HAS_ISTIO=0
kubectl -n kube-system   get ds  cilium  >/dev/null 2>&1 && HAS_CILIUM=1
kubectl -n istio-system  get deploy istiod >/dev/null 2>&1 && HAS_ISTIO=1
conflict=0
case "${PLATFORM}" in
    cilium)        [[ "${HAS_ISTIO}"  == "1" ]] && conflict=1 ;;
    istio)         [[ "${HAS_CILIUM}" == "1" ]] && conflict=1 ;;
    cilium-istio)  ;;  # any subset OK; we install whatever's missing
esac
if [[ "${conflict}" == "1" ]]; then
    cat >&2 <<EOF
ERROR: kind cluster '${CLUSTER}' is already running a different mesh layout than --platform=${PLATFORM}.
       Detected: cilium=${HAS_CILIUM}, istio=${HAS_ISTIO}
Recreate it:
    ./scripts/kind-down.sh
    ./scripts/kind-up.sh --platform ${PLATFORM}
EOF
    exit 1
fi

# 7) Gateway API CRDs FIRST. Both Cilium and Istio probe for Gateway API
#    resources at startup; missing CRDs cause the gateway-api subsystem to
#    stay disabled. Use the *experimental* channel because Cilium also
#    requires TLSRoute/TCPRoute (not in standard).
EXPECTED_CRDS=(
    gatewayclasses.gateway.networking.k8s.io
    gateways.gateway.networking.k8s.io
    httproutes.gateway.networking.k8s.io
    referencegrants.gateway.networking.k8s.io
    grpcroutes.gateway.networking.k8s.io
    tlsroutes.gateway.networking.k8s.io
)
NEED_CRD=0
for crd in "${EXPECTED_CRDS[@]}"; do
    if ! kubectl get crd "${crd}" >/dev/null 2>&1; then
        NEED_CRD=1
        break
    fi
done
if [[ "${NEED_CRD}" == "1" ]]; then
    echo "→ installing Gateway API ${GATEWAY_API_VERSION} experimental CRDs ..."
    kubectl apply --server-side --force-conflicts -f \
        "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml"
else
    echo "✓ Gateway API CRDs already installed"
fi

install_cilium() {
    # Argument: install-mode = "gateway" or "chained".
    #   gateway — Cilium owns the dataplane *and* GAMMA HTTPRoute enforcement.
    #   chained — Cilium provides only L3/L4; another mesh (Istio ambient)
    #             owns L7. We must disable Cilium's exclusive-CNI flag so
    #             istio-cni can install as a chained CNI plugin, and we
    #             leave gatewayAPI off so Istio is the sole GAMMA enforcer.
    local mode="${1:-gateway}"

    download_cilium_cli

    if ! kubectl -n kube-system get ds cilium >/dev/null 2>&1; then
        if [[ "${NEW_CLUSTER}" != "1" ]]; then
            cat >&2 <<EOF
ERROR: kind cluster '${CLUSTER}' exists but has no Cilium DaemonSet.
Recreate it:
    ./scripts/kind-down.sh
    ./scripts/kind-up.sh --platform ${PLATFORM}
EOF
            exit 1
        fi
        local cilium_args=(--version "${CILIUM_VERSION}")
        if [[ "${mode}" == "gateway" ]]; then
            echo "→ installing Cilium ${CILIUM_VERSION} with Gateway API (GAMMA) ..."
            cilium_args+=(
                --set kubeProxyReplacement=true
                --set gatewayAPI.enabled=true
                --set l7Proxy=true
            )
        else
            echo "→ installing Cilium ${CILIUM_VERSION} as L3/L4 CNI only (chained mode for istio-cni) ..."
            # cni.exclusive=false lets istio-cni write its own conf into
            # /etc/cni/net.d alongside cilium's.
            #
            # We deliberately DO NOT enable kubeProxyReplacement here, and
            # explicitly disable socketLB. Cilium's BPF socket-LB short-
            # circuits Service→Pod translation at the socket layer before
            # any netfilter hooks run, which means istio-cni's iptables
            # tproxy redirects don't fire and traffic skips ztunnel and the
            # waypoint — HTTPRoutes attached to the Service never apply.
            # Letting plain kube-proxy (still running per the kind config)
            # handle ClusterIP keeps the netfilter path intact for Istio.
            cilium_args+=(
                --set cni.exclusive=false
                --set socketLB.enabled=false
            )
        fi
        if ! "${CILIUM}" install "${cilium_args[@]}"; then
            echo "ERROR: 'cilium install' failed. Operator logs (last 80 lines):" >&2
            kubectl -n kube-system logs -l io.cilium/app=operator --tail=80 2>&1 | sed 's/^/  /' >&2 || true
            exit 1
        fi
        echo "→ waiting for Cilium to become ready ..."
        if ! "${CILIUM}" status --wait --wait-duration 3m; then
            echo "ERROR: Cilium did not become ready. Recent operator errors:" >&2
            kubectl -n kube-system logs -l io.cilium/app=operator --tail=120 2>&1 | grep -iE 'error|warn' | sed 's/^/  /' >&2 || true
            exit 1
        fi
    else
        echo "✓ Cilium already installed"
    fi

    if [[ "${mode}" == "gateway" ]]; then
        echo "→ verifying GatewayClass 'cilium' registration ..."
        for i in $(seq 1 20); do
            if kubectl get gatewayclass cilium >/dev/null 2>&1; then
                echo "✓ GatewayClass 'cilium' registered (GAMMA available)"
                return 0
            fi
            if [[ "$i" == "20" ]]; then
                echo "WARNING: GatewayClass 'cilium' not registered after 20s — GAMMA may be inactive." >&2
                kubectl -n kube-system logs -l io.cilium/app=operator --tail=200 2>&1 \
                    | grep -iE 'gateway|gamma' | tail -20 | sed 's/^/  /' >&2 || true
            fi
            sleep 1
        done
    fi
}

install_istio() {
    download_istioctl

    if ! kubectl -n istio-system get deploy istiod >/dev/null 2>&1; then
        if [[ "${NEW_CLUSTER}" != "1" ]]; then
            cat >&2 <<EOF
ERROR: kind cluster '${CLUSTER}' exists but has no istiod Deployment.
Recreate it:
    ./scripts/kind-down.sh
    ./scripts/kind-up.sh --platform istio
EOF
            exit 1
        fi
        echo "→ installing Istio ${ISTIO_VERSION} (ambient profile, GAMMA) ..."
        # ambient profile installs CNI + ztunnel + istiod, enabling waypoint
        # proxies for HTTPRoute mesh binding without sidecar injection.
        if ! "${ISTIOCTL}" install --skip-confirmation \
                --set profile=ambient \
                --set values.pilot.env.PILOT_ENABLE_ALPHA_GATEWAY_API=true; then
            echo "ERROR: 'istioctl install' failed. istiod logs (last 80 lines):" >&2
            kubectl -n istio-system logs -l app=istiod --tail=80 2>&1 | sed 's/^/  /' >&2 || true
            exit 1
        fi
        echo "→ waiting for istiod to become ready ..."
        kubectl -n istio-system rollout status deploy/istiod --timeout=3m
        kubectl -n istio-system rollout status ds/istio-cni-node --timeout=3m || true
        kubectl -n istio-system rollout status ds/ztunnel --timeout=3m || true
    else
        echo "✓ Istio already installed"
    fi

    echo "→ verifying GatewayClass 'istio' registration ..."
    for i in $(seq 1 30); do
        if kubectl get gatewayclass istio >/dev/null 2>&1; then
            echo "✓ GatewayClass 'istio' registered (GAMMA available)"
            return 0
        fi
        if [[ "$i" == "30" ]]; then
            echo "WARNING: GatewayClass 'istio' not registered after 30s — GAMMA may be inactive." >&2
            kubectl -n istio-system logs -l app=istiod --tail=200 2>&1 \
                | grep -iE 'gateway|gamma' | tail -20 | sed 's/^/  /' >&2 || true
        fi
        sleep 1
    done
}

case "${PLATFORM}" in
    cilium)        install_cilium gateway ;;
    istio)         install_istio ;;
    cilium-istio)  install_cilium chained && install_istio ;;
esac

case "${PLATFORM}" in
    cilium)       MESH_LINE="Cilium ${CILIUM_VERSION} (CNI + GAMMA)" ;;
    istio)        MESH_LINE="Istio ${ISTIO_VERSION} (ambient, GAMMA)" ;;
    cilium-istio) MESH_LINE="Cilium ${CILIUM_VERSION} (CNI, chained) + Istio ${ISTIO_VERSION} (ambient, GAMMA)" ;;
esac

cat <<EOF

================================================================
kind cluster '${CLUSTER}' is ready.

  PLATFORM   : ${PLATFORM}
  KUBECONFIG : ${KUBECONFIG}
  context    : kind-${CLUSTER}
  Mesh       : ${MESH_LINE}
  Gateway API: ${GATEWAY_API_VERSION}

Next:
    ./tilt-up.sh
================================================================
EOF
