<div align="center">

# route-prism

**컨텍스트 인지형 [GAMMA](https://gateway-api.sigs.k8s.io/mesh/gamma/) 라우팅 컨트롤러 — 쿠키 또는 헤더 한 줄로 어떤 Service 변종(variant)으로 갈지 결정합니다.**

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

## 무엇을 하나요

route-prism은 작은 CRD 3종으로 **멀티테넌트 트래픽 분기**, **개발자별 원격 라우팅**, **섀도우 트래픽** 시나리오에 안전하게 사용할 수 있는 라우팅 표면을 제공합니다.

- **`ContextRoute`** — [W3C Baggage](https://www.w3.org/TR/baggage/) 멤버 값을 기준으로 트래픽을 Service *variant* 들로 분기합니다. CR 하나가 HTTPRoute 하나, variant N개를 만듭니다.
- **`EdgeTransformation`** — 브라우저 쿠키를 엣지에서 Baggage로 변환해 인스트루멘트되지 않은 클라이언트(일반 사용자)도 컨텍스트를 전파할 수 있게 합니다. 옵션으로 **인페이지 위젯**을 주입해 사용자가 브라우저에서 직접 variant를 전환하게 할 수도 있습니다.
- **`RemoteRoute`** — variant 트래픽을 클러스터 *밖* 개발자 노트북으로 보내는 작은 Envoy 프록시를 자동 프로비저닝합니다. 한 명의 엔지니어가 다른 사용자에게 영향 없이 운영 트래픽 일부를 가져갈 수 있습니다.

모든 동작은 표준 [Gateway API GAMMA](https://gateway-api.sigs.k8s.io/mesh/gamma/) 위에서 이루어집니다 — route-prism은 메시를 대체하지 않고, 적절한 `HTTPRoute` 리소스를 만들어줄 뿐입니다.

## 동작 원리

```
                    ┌────────────┐                ┌──────────────┐
   request ─────►  │  Edge       │   baggage     │  ContextRoute │
   (쿠키 /     ▶   │  Trans-     │ ───────────►   │  HTTPRoute    │ ──► variant-A Service
    헤더)           │  formation │                │  (variant마다 │ ──► variant-B Service
                    └────────────┘                │   1개의 rule) │ ──► (기본) target Service
                                                  └──────────────┘            │
                                                          ▲                   │
                                                  RemoteRoute가              │
                                                  클러스터 밖으로 ───────────┘
```

1. 요청이 라우팅 힌트와 함께 도착합니다 — 분산 트레이싱이 이미 깔린 서비스라면 W3C `baggage` 헤더로, 일반 사용자라면 `EdgeTransformation`이 쿠키를 baggage로 다시 써서 전달합니다.
2. 대상 Service의 `ContextRoute`가 variant마다 매치 규칙(`baggage` 멤버 = variant 이름) 하나씩, 그리고 매치되지 않은 트래픽을 받는 catch-all 규칙을 갖는 `HTTPRoute`를 만듭니다.
3. variant는 클러스터 안의 평범한 Service가 될 수도 있고, `RemoteRoute`를 쓰면 개발자 PC로 포워딩하는 Envoy 프록시가 될 수도 있습니다. variant 태그가 없는 트래픽은 프록시를 보지 못하므로, 한 사람의 실험이 온콜을 깨우는 일은 없습니다.

전체 설계(변종 디스커버리, 전파 규칙, GAMMA 구현체 호환성)는 [Wiki](https://github.com/egoavara/route-prism/wiki)에 있습니다.

## 설치

**전제조건:** Kubernetes ≥ 1.28, [GAMMA를 지원](https://gateway-api.sigs.k8s.io/implementations/)하는 메시(Istio, Cilium, Linkerd 등), `kubectl`.

### Helm (권장)

```bash
helm install route-prism oci://ghcr.io/egoavara/charts/route-prism \
  --version <latest> \
  -n route-prism --create-namespace
```

버전 목록은 [차트 패키지 페이지](https://github.com/egoavara/route-prism/pkgs/container/charts%2Froute-prism)에서 확인하세요.

### 단일 YAML

```bash
kubectl apply -f https://github.com/egoavara/route-prism/releases/latest/download/route-prism.yaml
```

### 오퍼레이터 바이너리

[Releases 페이지](https://github.com/egoavara/route-prism/releases/latest)에서 다운로드 — `linux/amd64`, `linux/arm64`, `windows/amd64` 빌드 제공.

## 빠른 시작

### 1. Service를 variant로 분기

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

같은 namespace에서 `route-prism.egoavara.net/variant-of: checkout` 라벨을 가진 Service는 모두 라우팅 대상이 됩니다. 요청에 `baggage: x-route-prism=<service-name>`을 실어 보내면 그쪽으로 갑니다.

### 2. 브라우저도 참여시키기

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

쿠키 값 `<routingKey>:<variant>`이 업스트림 요청에서 Baggage로 변환됩니다. 위젯을 활성화하면 사용자에게 페이지 위에 떠 있는 variant 셀렉터가 노출됩니다.

### 3. 노트북으로 트래픽 터널링

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

`baggage: x-route-prism=alice`가 붙은 요청은 운영 환경을 우회해 alice의 노트북으로 갑니다. 다른 사용자는 절대 영향받지 않습니다.

## 문서

- **[Wiki](https://github.com/egoavara/route-prism/wiki)** — CRD별 상세, 전파 규칙, 메시 호환성 매트릭스, 운영 가이드.
- **API 레퍼런스** — `api/v1alpha1/*_types.go`로부터 생성 (Wiki 사이드바 참고).
- **예제** — `config/samples/`.

## 기여

이슈와 PR 환영합니다. [Kubebuilder](https://book.kubebuilder.io/)로 스캐폴딩되어 있고, 개발 워크플로우는 [`AGENTS.md`](AGENTS.md)에 정리되어 있습니다.

## 라이선스

[MIT License](LICENSE) © 2026 [egoavara](https://github.com/egoavara)
