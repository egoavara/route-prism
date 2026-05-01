# -*- mode: Python -*-
# Tiltfile for route-prism local dev loop.
#
# Setup steps the user runs MANUALLY (this Tiltfile does not bootstrap them):
#   1. ./hack/kind-up.sh [--platform cilium|istio]
#                              — create kind cluster + install chosen mesh
#                                + Gateway API CRDs (cilium is the default)
#   2. ./hack/tilt-up.sh        — launch Tilt with project-local KUBECONFIG
#
# Teardown:
#   ./hack/kind-down.sh         — delete the kind cluster
#
# The wrapper script sets KUBECONFIG=$(pwd)/bin/.kubeconfig before exec'ing
# tilt, so this Tiltfile only needs to verify Tilt landed on the right
# context, then proceed to deploy.

CLUSTER = "route-prism"
KIND_CONTEXT = "kind-" + CLUSTER
KIND = "./bin/kind"
KUBECONFIG_FILE = "bin/.kubeconfig"

# Refuse to run unless launched via the wrapper.
_kc_env = os.getenv("KUBECONFIG", "")
_pwd = str(local("pwd", quiet = True)).strip()
_expected_kc = _pwd + "/" + KUBECONFIG_FILE
if _kc_env != _expected_kc:
    fail("\n".join([
        "",
        "=" * 64,
        "Launch Tilt via the wrapper:",
        "",
        "    ./hack/tilt-up.sh",
        "",
        "(prerequisite: ./hack/kind-up.sh has created the cluster)",
        "=" * 64,
        "",
    ]))

if k8s_context() != KIND_CONTEXT:
    fail("Tilt sees k8s_context()=%r. Run ./hack/kind-up.sh then ./hack/tilt-up.sh." % k8s_context())

allow_k8s_contexts(KIND_CONTEXT)

# Sanity: cluster must already have Gateway API CRDs (installed by kind-up.sh).
if not str(local("kubectl get crd httproutes.gateway.networking.k8s.io --no-headers --ignore-not-found 2>/dev/null || true", quiet = True)).strip():
    fail("Gateway API CRDs missing. Run ./hack/kind-up.sh [--platform cilium|istio] to install the mesh + CRDs.")

# Sanity: at least one mesh-provided GatewayClass must be registered. The
# controller is mesh-agnostic (it produces GAMMA HTTPRoutes), but if neither
# cilium nor istio is on the cluster, the rendered HTTPRoutes are inert.
_gc_out = str(local("kubectl get gatewayclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true", quiet = True)).strip()
_gc_names = _gc_out.split() if _gc_out else []
if "cilium" in _gc_names:
    PLATFORM = "cilium"
elif "istio" in _gc_names:
    PLATFORM = "istio"
else:
    fail("No GatewayClass 'cilium' or 'istio' found. Run ./hack/kind-up.sh [--platform cilium|istio].")
print("→ detected mesh platform: %s" % PLATFORM)

# 1) Compile the manager + sample-tier binaries on the host. Done
#    synchronously at Tiltfile load so docker_build calls below always find
#    the binaries on disk; resource_deps only orders deploy, not image
#    build. (sample-tier is the tiny HTTP forwarder used by the 3-tier
#    devloop sample to chain web→api→db.)
print("→ initial dashboard build → internal/dashboard/dist ...")
local("cd web && pnpm install --prefer-offline && pnpm build", echo_off = False)
print("→ initial widget build → internal/widget/dist ...")
local("cd web/widget && pnpm install --prefer-offline && pnpm build", echo_off = False)
print("→ initial Go build → bin/manager-linux, bin/sample-tier-linux ...")
local("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager-linux cmd/main.go",
      echo_off = False)
local("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sample-tier-linux ./cmd/sample-tier",
      echo_off = False)

# Watch the web sources and rebuild the embedded dashboard. The output
# lands in internal/dashboard/dist, which the manager-compile resource
# already watches via its `internal` dep, so a frontend edit chains
# through to a new manager binary automatically.
local_resource(
    "dashboard-compile",
    cmd = "cd web && pnpm build",
    deps = ["web/src", "web/index.html", "web/vite.config.ts", "web/package.json"],
    ignore = ["web/node_modules", "web/dist", "web/widget"],
    labels = ["dev"],
)

# Watch widget sources and rebuild the embedded widget bundle. Output lands
# in internal/widget/dist, which manager-compile picks up via its `internal`
# dep so a widget edit chains to a fresh manager binary.
local_resource(
    "widget-compile",
    cmd = "cd web/widget && pnpm build",
    deps = ["web/widget/src", "web/widget/index.html", "web/widget/vite.config.ts", "web/widget/package.json"],
    ignore = ["web/widget/node_modules"],
    labels = ["dev"],
)

# Watch Go sources for incremental rebuilds. When these fire, the
# corresponding binary changes and docker_build (which watches it via
# `only=`) reruns; the affected Pod is replaced.
local_resource(
    "manager-compile",
    cmd = "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager-linux cmd/main.go",
    deps = ["cmd/main.go", "api", "internal", "go.mod", "go.sum"],
    resource_deps = ["dashboard-compile", "widget-compile"],
    labels = ["dev"],
)
local_resource(
    "sample-tier-compile",
    cmd = "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sample-tier-linux ./cmd/sample-tier",
    deps = ["cmd/sample-tier", "go.mod", "go.sum"],
    labels = ["dev"],
)

# 2) Build the images. Tilt loads them into kind automatically because the
#    context is kind-* and we use docker_build (Tilt detects kind and uses
#    `kind load docker-image`).
docker_build(
    "controller",                       # must match the kustomize image name
    context = ".",
    dockerfile_contents = """
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/manager-linux /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
""",
    only = ["bin/manager-linux"],
)
docker_build(
    "route-prism-sample-tier",         # referenced by test/devloop/sample.yaml
    context = ".",
    dockerfile_contents = """
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/sample-tier-linux /sample-tier
USER 65532:65532
ENTRYPOINT ["/sample-tier"]
""",
    only = ["bin/sample-tier-linux"],
)

# 3) Apply the kustomize manifests (CRDs, RBAC, manager Deployment, etc.).
k8s_yaml(kustomize("config/default"))

# 4) Group the controller resources for the Tilt UI.
k8s_resource(
    workload = "route-prism-controller-manager",
    new_name = "controller",
    labels = ["dev"],
    resource_deps = ["manager-compile"],
    # 8082 → routing API + Prometheus exporter (/metrics).
    # http://localhost:8082/metrics, http://localhost:8082/api/v1/service
    port_forwards = ["8082:8082"],
)

# 5) Apply demo workloads + sample CRs. The sample is a 3-tier topology
#    (web → api → db); each tier has stable + canary, a ContextRoute, and
#    a router-mode EdgeTransformation so cookie-based variant selection
#    works at every hop. Tilt auto-groups Deployments-as-workloads and
#    pulls matching Services into them; the ungrouped objects (Namespace
#    + CRs) need explicit assignment.
#
# Demo entry points (no Tilt port-forward — the kind cluster publishes
# NodePort 30080/30081/30083 on host-ports 8080/8081/8083 via
# extraPortMappings, so traffic enters via the Service IP and goes
# through the mesh's HTTPRoute / translator chain. That's what makes the
# entry tier honor the cookie. `kubectl port-forward` to a pod skips the
# mesh and would always hit stable.):
#
#   open http://localhost:8080/                                       → web HTML console (with widget)
#   curl                                       http://localhost:8080/api  → web stable chain
#   curl -H 'Cookie: x-route-prism=demo.web:web-canary'  http://localhost:8080/api  → web canary
#   curl                                       http://localhost:8081/api  → api direct
#   curl                                       http://localhost:8083/api  → db direct
k8s_yaml("test/devloop/sample.yaml")

DEMO_TIERS = ["web", "api", "db"]
for tier in DEMO_TIERS:
    k8s_resource(
        workload = tier,
        labels = ["dev"],
        resource_deps = ["controller", "sample-tier-compile"],
    )
    k8s_resource(
        workload = tier + "-canary",
        labels = ["dev"],
        resource_deps = ["controller", "sample-tier-compile"],
    )

# Edge proxy nginx: hostPorts (30080/30081/30083) proxy each tier's
# Service so external traffic enters via ClusterIP and triggers the
# GAMMA HTTPRoute / translator chain. kind extraPortMappings re-export
# these as host:8080/8081/8083.
k8s_resource(
    workload = "demo-edge",
    labels = ["dev"],
    resource_deps = ["controller"],
)

# All non-workload objects (Namespace + 3× ContextRoute + 2× EdgeTransformation)
# in a single grouped resource so the Tilt UI stays compact. db has no ET
# — its ContextRoute-rendered HTTPRoute matches baggage on its own.
k8s_resource(
    new_name = "demo-objects",
    objects = [
        "demo:Namespace:default",
        "demo-edge-conf:ConfigMap:demo",
        "web-route:ContextRoute:demo",
        "api-route:ContextRoute:demo",
        "db-route:ContextRoute:demo",
        "web-edge:EdgeTransformation:demo",
        "api-edge:EdgeTransformation:demo",
    ],
    labels = ["dev"],
    resource_deps = ["controller"],
)

# Both buttons run the same Ginkgo binary under test/e2e/, filtered by
# label. -count=1 disables the test cache so re-clicks always re-run.
# KUBECONFIG is inherited (Tilt sets it via tilt-up.sh) so the typed
# client-go reaches the kind cluster.

# One-click smoke check against the devloop sample (demo namespace).
# Exercises 4 scenarios (mesh/translator × no-cookie/with-cookie); every
# spec prints request / expected variant / observed pod.
local_resource(
    "check",
    cmd = "go test -tags routing -timeout 5m -v -count=1 ./test/e2e/ -ginkgo.label-filter=check -ginkgo.no-color=false",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    resource_deps = ["controller", "demo-objects"],
    labels = ["dev"],
)

# Cross-platform matrix regression. One disposable kind cluster per
# platform; every scenario group (g1, g3, g4, g7, g8) runs sequentially
# against that cluster, isolated by e2e-<group> namespaces the routing
# suite provisions in BeforeAll.
#
# Pre-flight (image build, manifest generation, tool prefetch, test-binary
# compile) runs once before parallel platform setups so they don't race.
# Per-platform logs land in ./bin/e2e-matrix-logs/platform-<platform>.log.
#
# Buttons:
#   e2e-matrix        — all three platforms in parallel (~10–15 min)
#   e2e-cilium        — only cilium               (single platform)
#   e2e-istio         — only istio
#   e2e-cilium-istio  — only the chained combo
#
# Single-platform buttons reuse the same orchestrator binary; pre-flight
# (image build / manifests / prefetch / compile) is the same regardless of
# how many platforms are selected, so re-running with the same image tag
# is fast (Docker / Go caches).
#
# CLI escape hatches for narrow runs:
#   go run ./cmd/e2e-matrix --groups=g1 --platforms=cilium-istio --keep
#   go run ./cmd/e2e-matrix --skip-build           # reuse last image
local_resource(
    "e2e-matrix",
    cmd = "go run ./cmd/e2e-matrix",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    labels = ["e2e"],
)
local_resource(
    "e2e-cilium",
    cmd = "go run ./cmd/e2e-matrix --platforms=cilium",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    labels = ["e2e"],
)
local_resource(
    "e2e-istio",
    cmd = "go run ./cmd/e2e-matrix --platforms=istio",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    labels = ["e2e"],
)
local_resource(
    "e2e-cilium-istio",
    cmd = "go run ./cmd/e2e-matrix --platforms=cilium-istio",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    labels = ["e2e"],
)

# Background sweeper: periodically prune dangling container images that
# accumulate as Tilt rebuilds `controller` / `route-prism-sample-tier` and
# re-runs `kind load docker-image`. Each load tags the new image with the
# same name and leaves the previous SHA dangling — both on the host Docker
# daemon AND inside the kind node's containerd image store, which has no
# automatic GC. Over a long `tilt up` session this inflates the kind node
# container's RSS and disk footprint (visible from the host as growing
# memory use).
#
# Implemented as `serve_cmd` so Tilt keeps the loop alive for the lifetime
# of `tilt up` and SIGTERMs it on shutdown. The first sweep waits one full
# interval to avoid racing the initial image build/load.
PRUNE_INTERVAL_SECONDS = 300
local_resource(
    "prune-images",
    serve_cmd = """
set -eu
NODE="route-prism-control-plane"
INTERVAL=%d

echo "[prune-images] sweeper started; interval=${INTERVAL}s, node=${NODE}"
while true; do
    sleep "$INTERVAL"
    ts="$(date '+%%Y-%%m-%%dT%%H:%%M:%%S')"

    if ! docker inspect -f '{{.State.Running}}' "$NODE" >/dev/null 2>&1; then
        echo "[$ts] kind node $NODE not running; skipping sweep"
        continue
    fi

    before=$(docker exec "$NODE" crictl images -q 2>/dev/null | wc -l)
    docker exec "$NODE" crictl rmi --prune >/dev/null 2>&1 || true
    after=$(docker exec "$NODE" crictl images -q 2>/dev/null | wc -l)

    host_pruned=$(docker image prune -f --filter "dangling=true" 2>/dev/null \\
        | awk '/Total reclaimed space/ {print $0}')

    mem=$(docker stats --no-stream --format '{{.MemUsage}}' "$NODE" 2>/dev/null || echo "?")

    echo "[$ts] kind-node images ${before}→${after}; host: ${host_pruned:-nothing}; node mem: ${mem}"
done
""" % PRUNE_INTERVAL_SECONDS,
    labels = ["dev"],
)

# Manual button: emit explicit `kubectl config set-cluster / set-credentials
# / set-context` commands. Designed for cross-environment use — e.g. paste
# the output into a Windows PowerShell that has its own kubectl, so it can
# reach the kind cluster running inside WSL2/Docker Desktop. The kind API
# server is published to 127.0.0.1:<port> on the Docker host, which WSL2
# forwards to the Windows host transparently.
local_resource(
    "access-info",
    cmd = """
set -eu
KC="$(pwd)/bin/.kubeconfig"
CTX="kind-route-prism"

if [ ! -s "$KC" ]; then
    echo "kubeconfig not found at $KC — start Tilt via ./hack/tilt-up.sh first." >&2
    exit 1
fi

SERVER=$(kubectl --kubeconfig "$KC" config view --raw --minify \\
    -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl --kubeconfig "$KC" config view --raw --minify \\
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
CERT=$(kubectl --kubeconfig "$KC" config view --raw --minify \\
    -o jsonpath='{.users[0].user.client-certificate-data}')
KEY=$(kubectl --kubeconfig "$KC" config view --raw --minify \\
    -o jsonpath='{.users[0].user.client-key-data}')

cat <<EOF

== Option A — local shell, no system kubeconfig changes =====================
  export KUBECONFIG="$(pwd)/bin/.kubeconfig"
  kubectl get nodes

== Option B — install context into your kubeconfig (run anywhere) ===========
Run these from the shell whose kubectl/kubeconfig you want to use. Works
identically on bash, zsh, PowerShell, and cmd. From Windows: $SERVER is
reachable because Docker Desktop publishes the kind API port to the host
and WSL2 forwards localhost transparently.

  kubectl config set-cluster $CTX --server=$SERVER
  kubectl config set clusters.$CTX.certificate-authority-data $CA
  kubectl config set-credentials $CTX
  kubectl config set users.$CTX.client-certificate-data $CERT
  kubectl config set users.$CTX.client-key-data $KEY
  kubectl config set-context $CTX --cluster=$CTX --user=$CTX
  kubectl config use-context $CTX
  kubectl get nodes
EOF
""",
    auto_init = False,
    trigger_mode = TRIGGER_MODE_MANUAL,
    labels = ["dev"],
)

print("""
================================================================
route-prism dev loop ready.

  Tilt UI : http://localhost:10350
  KUBECONFIG (for this shell):
      export KUBECONFIG=$(pwd)/bin/.kubeconfig

  Click the 'access-info' resource in the UI for kubectl/docker/kind
  one-liners (or run: tilt trigger access-info).
================================================================
""")
