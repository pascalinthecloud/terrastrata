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
- Optional [module registry](#module-registry) (`MODULES_ENABLED`): caches
  registry modules too, though unlike providers it requires rewriting each
  module's `source` — Terraform has no mirror protocol for modules

---

## Quick start

### 1. Deploy to Kubernetes

With raw manifests:

```bash
# Optionally fill in your S3 credentials in deploy/k8s/manifests.yaml first
kubectl apply -f deploy/k8s/manifests.yaml
```

Or with Helm, straight from the OCI registry (chart is cosign-signed like the image):

```bash
helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata \
  --version 0.3.0 \
  --namespace tf-mirror --create-namespace
# With durable S3 cache:
#   --set s3.enabled=true --set s3.bucket=tf-mirror \
#   --set s3.endpoint=https://s3.de.io.cloud.ovh.net --set s3.region=de \
#   --set s3.accessKey=... --set s3.secretKey=...
```

(From a checkout, `helm install tf-mirror deploy/helm/terrastrata ...` works the same.)

### 2. Configure Terraform agents

Add to `~/.terraformrc` on each agent (or inject via CI pipeline):

```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/" # your Ingress/Gateway hostname
    include = ["registry.terraform.io/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*"]
  }
}
```

> **The mirror URL must be `https`** — Terraform refuses a plaintext
> `network_mirror` URL. terrastrata itself serves plain HTTP, so point agents
> at the TLS-terminating Ingress/Gateway in front of it (see the Ingress
> options in the chart), with a certificate the agents trust.

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
| `MIRROR_HOSTNAME` | _(host of `UPSTREAM_BASE`)_ | Registry hostname this mirror serves (the `{hostname}` path segment clients request); requests for any other hostname return 404. Only set it when clients address providers by a different name than the upstream URL |
| `S3_BUCKET` | _(empty)_ | S3 bucket name. **Leave empty to disable S3** — local filesystem cache only |
| `S3_PREFIX` | `tf-mirror` | Key prefix within the S3 bucket |
| `S3_ENDPOINT` | _(empty)_ | Custom S3 endpoint (OVH, MinIO, etc.) |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_ACCESS_KEY` | _(empty)_ | S3 access key. Set together with `S3_SECRET_KEY`, or leave **both** empty to use the AWS default credential chain (IRSA, instance profile, env) |
| `S3_SECRET_KEY` | _(empty)_ | S3 secret key |
| `AUTH_TOKEN` | _(empty)_ | Optional bearer token required on mirror endpoints. **Leave empty to disable auth** (internal mode) |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `INDEX_TTL` | `10m` | How long a cached provider **versions index** is served before being revalidated upstream (Go duration, e.g. `30m`, `1h`). `0` disables expiry. Archives and zips are immutable and never expire |
| `PREWARM_PROVIDERS` | _(empty)_ | Comma-separated providers to warm into the cache at startup, each `[host/]namespace/type[@version]`. A bare provider warms only its versions index; `@version` also warms that version's archives and zips. Empty disables pre-warming |
| `PREWARM_PLATFORMS` | `linux_amd64` | Comma-separated `os_arch` platforms to warm zips for (only applies to `@version` entries) |
| `MODULES_ENABLED` | `false` | Serve the [module registry](#module-registry) protocol (adds `/.well-known/terraform.json` and `/v1/modules/`) |
| `MODULES_UPSTREAM_BASE` | _(value of `UPSTREAM_BASE`)_ | Upstream module registry. Only set it when modules come from a different host than providers |

> **Note on `AUTH_TOKEN`:** Terraform's `network_mirror` client does not send
> authentication headers, so bearer auth is meant for an API gateway that injects
> the header, or for non-Terraform consumers. For Terraform clients, rely on
> network policy / ingress controls instead. `/health` and `/metrics` are always
> unauthenticated.
>
> Module registry endpoints are different: Terraform *does* send credentials from
> a `credentials` block to them, so `AUTH_TOKEN` works for modules. The module
> **archive** endpoint stays unauthenticated regardless, because Terraform
> attaches credentials only to registry requests and not to the archive download
> that follows.

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
ghcr.io/pascalinthecloud/terrastrata:0.3.1     # exact version
ghcr.io/pascalinthecloud/terrastrata:0.3       # major.minor
ghcr.io/pascalinthecloud/terrastrata:sha-<sha> # by commit
```

Images are **multi-arch** (`linux/amd64`, `linux/arm64`), built on a distroless
runtime, and ship with an **SBOM** and **build provenance**. Each is **signed
with cosign** (keyless / Sigstore) — verify before deploying:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/pascalinthecloud/terrastrata/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pascalinthecloud/terrastrata:0.3.1
```

Pin by digest in production. To cut a release:

```bash
git tag v0.3.0 && git push origin v0.3.0
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

Modules (when enabled) live beside it under a `_modules/` root, which no provider
hostname can collide with:

```
cache/
└── _modules/
    └── claranet/
        └── regions/
            └── azurerm/
                ├── versions.json        # version list
                └── 8.0.6/
                    ├── location.json    # resolved upstream source
                    └── archive          # the module tarball
```

---

## Module registry

Set `MODULES_ENABLED=true` to also serve Terraform's
[module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol).

**This works differently from providers, and the difference matters.** Terraform
has no *mirror* protocol for modules — only the registry protocol. So terrastrata
does not transparently intercept module traffic the way it does for providers:
it becomes a registry that clients address directly, which means **rewriting the
`source` of every module you want cached**:

```hcl
module "regions" {
  # was: claranet/regions/azurerm
  source  = "tf-mirror.internal/claranet/regions/azurerm"
  version = "8.0.6"
}
```

Terraform discovers the API via `https://tf-mirror.internal/.well-known/terraform.json`,
so the mirror must be reachable over **https** under a hostname containing a dot
(Terraform rejects `localhost:8443` as a registry hostname).

There is nothing to configure on the client beyond the source address. Only
registry-sourced modules can be cached — `git::`, local paths, and other
[module sources](https://developer.hashicorp.com/terraform/language/modules/sources)
never touch a registry and are unaffected.

### What gets cached

On a download, the upstream registry answers with an `X-Terraform-Get` pointing
at the module's real source. terrastrata rewrites that to its own archive
endpoint, then fetches, caches, and serves the archive itself.

In practice the public registry returns a `git::` source for every module
(`git::https://github.com/OWNER/REPO?ref=<commit>`), despite the protocol docs
showing an https tarball. terrastrata maps GitHub sources onto the equivalent
`codeload.github.com` tarball, so no git client is involved. It also strips the
single wrapper directory GitHub tarballs add, because Terraform does not expand
the go-getter `//*` subdir glob for registry modules.

Sources it cannot fetch — a non-GitHub `git::` host, `ssh://`, `s3::`, `hg::` —
are **passed through unchanged** with `X-Cache: BYPASS`. `terraform init` still
works wherever the client can reach the original source, but nothing is cached.
Watch `terrastrata_module_downloads_total{outcome="bypass"}` to see whether that
is happening to you.

> **No checksums.** The module registry protocol publishes none, so module
> archives cannot be verified the way provider zips are (which terrastrata checks
> against the registry-published SHA-256 before caching). Integrity rests on the
> https fetch plus a 512 MiB size cap. If that trade-off is unacceptable, leave
> `MODULES_ENABLED` off.

## Observability

- `GET /health` — liveness/readiness probe (always unauthenticated)
- `GET /metrics` — Prometheus metrics (always unauthenticated), including:
  - `terrastrata_cache_lookups_total{resource,result}` — cache hit/miss by resource
  - `terrastrata_http_requests_total{route,code}` and `terrastrata_http_request_duration_seconds{route}`
  - `terrastrata_versions_index_total{outcome}` — versions-index freshness:
    `fresh` (within TTL), `revalidated` (refetched), `stale` (served after an
    upstream failure — **alert on a rising rate here**), `error` (no fallback)
  - `terrastrata_module_downloads_total{outcome}` — module downloads by outcome:
    `cached` (served from terrastrata's own archive), `bypass` (a source it
    cannot cache, passed through — **a rising rate means modules are not
    actually being cached**), `error`
  - `terrastrata_prewarm_total{resource,result}` — startup pre-warm successes/failures
  - `terrastrata_cache_size_bytes` (gauge), `terrastrata_cache_evictions_total`,
    `terrastrata_cache_evicted_bytes_total` — local cache size and eviction activity
  - plus standard Go runtime and process collectors
- Structured JSON access logs on stdout, one line per request, with a
  per-request `X-Request-Id`

**Scraping:** with an operator-based stack (kube-prometheus-stack,
VictoriaMetrics operator) enable the chart's ServiceMonitor —
`--set serviceMonitor.enabled=true` (plus `serviceMonitor.labels` if your
Prometheus selects monitors by label). The `prometheus.io/*` pod annotations
the chart also sets only work with classic annotation-based scrape configs.

A ready-to-run local stack (terrastrata + Prometheus + Grafana with a
provisioned dashboard, plus optional MinIO and a TLS front for real `terraform`
clients) lives in [`deploy/local/`](deploy/local/) for exercising the mirror
under load and watching these metrics over time.

---

## Kubernetes notes

- **Replicas: 1 by default** — the default PVC is `ReadWriteOnce`, so the chart pins one replica and uses the `Recreate` strategy. See **High availability** below to run multiple replicas.
- **PVC size** — `20Gi` default. `hashicorp/azurerm` alone can reach 30–50 GB if all versions are cached. Size accordingly, and set `CACHE_MAX_BYTES` (e.g. a few GB below the PVC size) so terrastrata evicts least-recently-used artifacts instead of filling the volume.
- **TLS** — terrastrata serves plain HTTP internally. Terminate TLS at your Ingress or Gateway controller.
- **Ingress** — an example Ingress resource is included (commented out) in `deploy/k8s/manifests.yaml`.
- **Secrets** — for production, prefer `auth.existingSecret` / `s3.existingSecret` over inline chart values: inline credentials end up in Helm release history and shell history. With S3 on AWS, you can skip credentials entirely and use IRSA (`serviceAccount.annotations` + empty `s3.accessKey`).

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
- [x] Prometheus metrics endpoint
- [x] Helm chart
- [x] Size-bounded LRU cache eviction (`CACHE_MAX_BYTES`)
- [x] Request coalescing for concurrent cold requests
- [x] Multi-replica high availability (S3-backed)
- [x] Support for module registry protocol (`MODULES_ENABLED`)

---

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow and conventions. By participating you agree to the
[Code of Conduct](CODE_OF_CONDUCT.md). Security issues should be reported
privately per the [Security Policy](SECURITY.md). Notable changes are recorded in
the [Changelog](CHANGELOG.md).

---

## License

Apache 2.0
