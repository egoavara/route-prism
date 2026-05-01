<div align="center">

# route-prism

**Context-aware [GAMMA](https://gateway-api.sigs.k8s.io/mesh/gamma/) routing for Kubernetes — one cookie or header decides which variant of a Service the request lands on.**

[English](README.md) | [한국어](README.ko.md) | [Wiki](https://github.com/egoavara/route-prism/wiki)

[![License](https://img.shields.io/github/license/egoavara/route-prism?color=blue)](LICENSE)
[![Release](https://img.shields.io/github/v/release/egoavara/route-prism?include_prereleases&sort=semver)](https://github.com/egoavara/route-prism/releases)
[![Container](https://img.shields.io/badge/ghcr.io-egoavara%2Froute--prism-2496ED?logo=docker&logoColor=white)](https://github.com/egoavara/route-prism/pkgs/container/route-prism)
[![Helm Chart](https://img.shields.io/badge/Helm-OCI-0F1689?logo=helm&logoColor=white)](https://github.com/egoavara/route-prism/pkgs/container/charts%2Froute-prism)
[![Go Version](https://img.shields.io/github/go-mod/go-version/egoavara/route-prism)](go.mod)
[![Gateway API](https://img.shields.io/badge/Gateway%20API-GAMMA-326CE5?logo=kubernetes&logoColor=white)](https://gateway-api.sigs.k8s.io/mesh/gamma/)
[![Release Workflow](https://github.com/egoavara/route-prism/actions/workflows/release.yml/badge.svg)](https://github.com/egoavara/route-prism/actions/workflows/release.yml)

</div>

---

## What it does

route-prism turns three small Kubernetes CRDs into a routing surface that's safe to ship for **multi-tenant**, **per-developer**, and **shadow-traffic** scenarios:

- **`ContextRoute`** — splits traffic into Service *variants* based on a [W3C Baggage](https://www.w3.org/TR/baggage/) member. One CR, one HTTPRoute, N variants.
- **`EdgeTransformation`** — translates a browser cookie into Baggage at the edge so end users (not just instrumented services) can carry context. Optionally injects an **in-page widget** so users can flip variants from their own browser.
- **`RemoteRoute`** — provisions a small Envoy proxy that funnels variant traffic *out* of the cluster to a developer's laptop. Lets one engineer take over a slice of production traffic without touching anyone else.

Everything is built on standard [Gateway API GAMMA](https://gateway-api.sigs.k8s.io/mesh/gamma/) (mesh service routing) — route-prism does not replace your mesh, it just emits the right `HTTPRoute` resources.

## How it works

```
                    ┌────────────┐                ┌──────────────┐
   request ─────►   │  Edge       │   baggage     │  ContextRoute │
   (cookie /    ▶  │  Trans-     │ ───────────►   │  HTTPRoute    │ ──► variant-A Service
    header)        │  formation │                │  (1 rule per  │ ──► variant-B Service
                    └────────────┘                │    variant)   │ ──► (default) target Service
                                                  └──────────────┘            │
                                                          ▲                   │
                                                  RemoteRoute proxies         │
                                                  off-cluster traffic ────────┘
```

1. A request arrives carrying a routing hint — either as a W3C `baggage` header (services already do distributed tracing) or as a cookie that an `EdgeTransformation` rewrites into baggage.
2. The `ContextRoute` for the target Service emits an `HTTPRoute` with one match rule per variant (`baggage` member equals that variant's name) plus a catch-all that goes back to the default Service.
3. Variants can be ordinary Services in-cluster, or — with `RemoteRoute` — an Envoy proxy that forwards out to a developer's machine. Traffic without the variant tag never sees the proxy; one developer's experiment can't accidentally page the on-call.

The full design (variant discovery, propagation rules, GAMMA implementer compatibility) lives in the [Wiki](https://github.com/egoavara/route-prism/wiki).

## Install

**Prerequisites:** Kubernetes ≥ 1.28, a [GAMMA-supporting](https://gateway-api.sigs.k8s.io/implementations/) mesh (Istio, Cilium, Linkerd…), `kubectl`.

### Helm (recommended)

```bash
helm install route-prism oci://ghcr.io/egoavara/charts/route-prism \
  --version <latest> \
  -n route-prism --create-namespace
```

Browse versions at [the chart package page](https://github.com/egoavara/route-prism/pkgs/container/charts%2Froute-prism).

### Single-file YAML

```bash
kubectl apply -f https://github.com/egoavara/route-prism/releases/latest/download/route-prism.yaml
```

### Operator binary (for `kubectl --kubeconfig` style runs)

Download from the [Releases page](https://github.com/egoavara/route-prism/releases/latest) — pre-built for `linux/amd64`, `linux/arm64`, and `windows/amd64`.

## Quickstart

### 1. Split a Service into variants

```yaml
apiVersion: route-prism.egoavara.net/v1alpha1
kind: ContextRoute
metadata:
  name: checkout
  namespace: shop
spec:
  target:
    service:
      name: checkout
  variants:
    selector:
      matchLabels:
        route-prism.egoavara.net/variant-of: checkout
```

Any Service in the namespace carrying `route-prism.egoavara.net/variant-of: checkout` is now a routing target. Send a request with `baggage: x-route-prism=<service-name>` and it lands there.

### 2. Let browsers participate

```yaml
apiVersion: route-prism.egoavara.net/v1alpha1
kind: EdgeTransformation
metadata:
  name: checkout-edge
  namespace: shop
spec:
  mode: router
  sourceCookie: x-route-prism
  target:
    service:
      name: checkout
  widgetInjection:
    enable: true
```

The cookie value `<routingKey>:<variant>` becomes Baggage on the upstream request. The optional widget gives users a floating in-page selector.

### 3. Tunnel traffic to a laptop

```yaml
apiVersion: route-prism.egoavara.net/v1alpha1
kind: RemoteRoute
metadata:
  name: alice
  namespace: shop
spec:
  contextRouteRef:
    name: checkout
  upstreams:
    - url: https://alice-laptop.tailnet.ts.net:8443
```

Now a request with `baggage: x-route-prism=alice` skips production and hits Alice's machine. Other users never see it.

## Documentation

- **[Wiki](https://github.com/egoavara/route-prism/wiki)** — deep dives on each CRD, propagation rules, mesh compatibility matrix, and runbooks.
- **API reference** — generated from `api/v1alpha1/*_types.go` (see Wiki sidebar).
- **Examples** — `config/samples/`.

## Contributing

Issues and PRs are welcome. The project is scaffolded with [Kubebuilder](https://book.kubebuilder.io/) — see [`AGENTS.md`](AGENTS.md) for the dev workflow.

## License

[MIT License](LICENSE) © 2026 [egoavara](https://github.com/egoavara)
