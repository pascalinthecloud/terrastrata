# terrastrata

> Pull-through Terraform provider cache registry

**terrastrata** is a lightweight self-hosted proxy that implements the [Terraform Network Mirror Protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol). It fetches providers from the public registry on demand, caches them locally and in S3-compatible object storage, and serves subsequent requests entirely from cache — no repeated upstream calls, no internet dependency after first use.

---

## Why

- You are tired of GitHub outages causing terraform init to fail mid-pipeline for no reason on your end
- Your CI/CD agents run in an isolated or bandwidth-constrained network
- `registry.terraform.io` is slow, rate-limited, or simply unreachable
- You want reproducible `terraform init` without pinning provider zips manually
- You need a durable provider cache that survives pod restarts

---

## How it works

```
terraform init
      │
      ▼
 terrastrata
      │  cache HIT  → serve from local volume
      │  cache MISS → fetch from registry.terraform.io
      │               ├─ write to local PVC  (fast, ephemeral)
      │               └─ async write to S3   (durable, survives restarts)
      ▼
registry.terraform.io   (only on first request per version)
```

Cache lookup order: **local PVC → S3 (if enabled) → upstream registry**. When S3 is enabled, it automatically warms the local volume on pod restart so nothing is re-fetched from the internet. Without S3, only the local PVC is used.

---

## Features

- Implements the Terraform Network Mirror Protocol — drop-in replacement, no Terraform changes needed
- Pull-through: providers are fetched and cached on first use, never pre-downloaded
- Request coalescing: when many agents request the same uncached provider at once, a single upstream fetch is performed and shared — no thundering herd against `registry.terraform.io`
- Dual-layer cache: local filesystem (fast) + optional S3-compatible object storage (durable)
- When S3 is enabled, works with any S3-compatible backend: AWS S3, OVH Object Storage, MinIO, Azure Blob (via gateway)
- Kubernetes-native: ships with manifests and a lightweight container image
- Zero auth required for internal network deployments
- Optional pre-warm on startup: seed the cache from a provider list (`PREWARM_PROVIDERS`) so CI pipelines hit a warm cache on first run
- Versions index is revalidated on a configurable TTL so new provider releases appear; if the upstream registry is down at revalidation time, the last-known-good list is served stale (`X-Cache: STALE`) instead of failing
- `X-Cache: HIT/MISS/STALE` response headers for observability
- `/health` endpoint for liveness/readiness probes

---

## Quick start

### 1. Deploy to Kubernetes

With raw manifests:

```bash
# Optionally fill in your S3 credentials in deploy/k8s/manifests.yaml first
kubectl apply -f deploy/k8s/manifests.yaml
```

Or with Helm:

```bash
helm install tf-mirror deploy/helm/terrastrata \
  --namespace tf-mirror --create-namespace
# With durable S3 cache:
#   --set s3.enabled=true --set s3.bucket=tf-mirror \
#   --set s3.endpoint=https://s3.de.io.cloud.ovh.net --set s3.region=de \
#   --set s3.accessKey=... --set s3.secretKey=...
```

### 2. Configure Terraform agents

Add to `~/.terraformrc` on each agent (or inject via CI pipeline):

```hcl
provider_installation {
  network_mirror {
    url     = "http://tf-mirror.tf-mirror.svc.cluster.local/"
    include = ["registry.terraform.io/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*"]
  }
}
```

### 3. Run `terraform init` as normal

```bash
terraform init
# Initializing provider plugins...
# - Installing hashicorp/azurerm v3.110.0 from http://terrastrata.internal/...
```

---

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address and port to listen on |
| `CACHE_DIR` | `/cache` | Local filesystem cache directory |
| `CACHE_MAX_BYTES` | _(empty)_ | Size budget for the local cache (e.g. `20GB`, `512Mi`, or raw bytes). When exceeded, least-recently-used files are evicted down to ~90% of the budget. Empty/`0` disables eviction (unbounded) |
| `UPSTREAM_BASE` | `https://registry.terraform.io` | Upstream registry base URL |
| `S3_BUCKET` | _(empty)_ | S3 bucket name. **Leave empty to disable S3** — local filesystem cache only |
| `S3_PREFIX` | `tf-mirror` | Key prefix within the S3 bucket |
| `S3_ENDPOINT` | _(empty)_ | Custom S3 endpoint (OVH, MinIO, etc.) |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_ACCESS_KEY` | _(empty)_ | S3 access key |
| `S3_SECRET_KEY` | _(empty)_ | S3 secret key |
| `AUTH_TOKEN` | _(empty)_ | Optional bearer token required on mirror endpoints. **Leave empty to disable auth** (internal mode) |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `INDEX_TTL` | `10m` | How long a cached provider **versions index** is served before being revalidated upstream (Go duration, e.g. `30m`, `1h`). `0` disables expiry. Archives and zips are immutable and never expire |
| `PREWARM_PROVIDERS` | _(empty)_ | Comma-separated providers to warm into the cache at startup, each `[host/]namespace/type[@version]`. A bare provider warms only its versions index; `@version` also warms that version's archives and zips. Empty disables pre-warming |
| `PREWARM_PLATFORMS` | `linux_amd64` | Comma-separated `os_arch` platforms to warm zips for (only applies to `@version` entries) |

> **Note on `AUTH_TOKEN`:** Terraform's `network_mirror` client does not send
> authentication headers, so bearer auth is meant for an API gateway that injects
> the header, or for non-Terraform consumers. For Terraform clients, rely on
> network policy / ingress controls instead. `/health` and `/metrics` are always
> unauthenticated.

### OVH Object Storage example

```yaml
- name: S3_ENDPOINT
  value: "https://s3.de.io.cloud.ovh.net"
- name: S3_REGION
  value: "de"
- name: S3_BUCKET
  value: "tf-mirror"
```

---

## Building

```bash
# Build binary (or: make build -> ./bin/terrastrata)
go build -o terrastrata ./cmd/terrastrata

# Run the test suite (race detector)
make test

# Build container image
docker build -t your-registry/terrastrata:latest .

# Push
docker push your-registry/terrastrata:latest
```

---

## Container images

Released images are published to GitHub Container Registry on every version tag:

```
ghcr.io/pascalinthecloud/terrastrata:0.1.0     # exact version
ghcr.io/pascalinthecloud/terrastrata:0.1       # major.minor
ghcr.io/pascalinthecloud/terrastrata:sha-<sha> # by commit
```

Images are **multi-arch** (`linux/amd64`, `linux/arm64`), built on a distroless
runtime, and ship with an **SBOM** and **build provenance**. Each is **signed
with cosign** (keyless / Sigstore) — verify before deploying:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/pascalinthecloud/terrastrata/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pascalinthecloud/terrastrata:0.1.0
```

Pin by digest in production. To cut a release:

```bash
git tag v0.1.0 && git push origin v0.1.0
# the Release workflow builds, pushes, signs, and drafts the GitHub release
```

---

## Cache structure

terrastrata stores artifacts in the Terraform Network Mirror Protocol directory layout:

```
cache/
└── registry.terraform.io/
    └── hashicorp/
        └── azurerm/
            ├── index.json                         # versions list
            ├── 3.110.0.json                       # archives metadata for 3.110.0
            └── 3.110.0/
                └── download/
                    └── linux_amd64/
                        └── terraform-provider-azurerm_3.110.0_linux_amd64.zip
```

This matches the [network mirror protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol)
endpoints: `index.json` (versions) and `<version>.json` (archives). The same
structure is mirrored under your configured S3 prefix.

## Observability

- `GET /health` — liveness/readiness probe (always unauthenticated)
- `GET /metrics` — Prometheus metrics (always unauthenticated), including:
  - `terrastrata_cache_lookups_total{resource,result}` — cache hit/miss by resource
  - `terrastrata_http_requests_total{route,code}` and `terrastrata_http_request_duration_seconds{route}`
  - `terrastrata_versions_index_total{outcome}` — versions-index freshness:
    `fresh` (within TTL), `revalidated` (refetched), `stale` (served after an
    upstream failure — **alert on a rising rate here**), `error` (no fallback)
  - `terrastrata_prewarm_total{resource,result}` — startup pre-warm successes/failures
  - `terrastrata_cache_size_bytes` (gauge), `terrastrata_cache_evictions_total`,
    `terrastrata_cache_evicted_bytes_total` — local cache size and eviction activity
  - plus standard Go runtime and process collectors
- Structured JSON access logs on stdout, one line per request, with a
  per-request `X-Request-Id`

---

## Kubernetes notes

- **Replicas: 1 by default** — the default PVC is `ReadWriteOnce`, so the chart pins one replica and uses the `Recreate` strategy. See **High availability** below to run multiple replicas.
- **PVC size** — `20Gi` default. `hashicorp/azurerm` alone can reach 30–50 GB if all versions are cached. Size accordingly, and set `CACHE_MAX_BYTES` (e.g. a few GB below the PVC size) so terrastrata evicts least-recently-used artifacts instead of filling the volume.
- **TLS** — terrastrata serves plain HTTP internally. Terminate TLS at your Ingress or Gateway controller.
- **Ingress** — an example Ingress resource is included (commented out) in `k8s/manifests.yaml`.

### High availability

A `ReadWriteOnce` PVC cannot be shared, so HA runs **multiple replicas in
S3-backed mode**: each pod keeps its own ephemeral local cache (an `emptyDir`)
and shares durability through the S3 layer that every replica reads and writes.

```bash
helm install tf-mirror deploy/helm/terrastrata \
  --namespace tf-mirror --create-namespace \
  --set replicaCount=3 \
  --set persistence.enabled=false \
  --set s3.enabled=true --set s3.bucket=tf-mirror \
  --set s3.endpoint=https://s3.de.io.cloud.ovh.net --set s3.region=de \
  --set s3.accessKey=... --set s3.secretKey=... \
  --set podDisruptionBudget.enabled=true
```

The chart then switches to a rolling-update `Deployment`, spreads replicas across
nodes (a soft pod anti-affinity, overridable via `affinity` /
`topologySpreadConstraints`), and renders a `PodDisruptionBudget`. Requesting
`replicaCount > 1` while keeping a `ReadWriteOnce` PVC is rejected at render time
with a clear message — switch to S3-backed mode or a `ReadWriteMany` storage
class. Request coalescing is per-pod, so a cold provider is fetched at most once
per replica rather than once per request.

---

## Roadmap

- [x] Cache TTL / revalidation for index.json (versions list)
- [x] Pre-warm mode: seed cache from a provider list on startup
- [ ] Prometheus metrics endpoint
- [ ] Helm chart
- [ ] Support for module registry protocol

---

## License

Apache 2.0
