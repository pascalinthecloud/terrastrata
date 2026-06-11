# CLAUDE.md — terrastrata project context

This file provides context for AI assistants working on this project.

---

## Project overview

**terrastrata** is a self-hosted pull-through cache proxy implementing the [Terraform Network Mirror Protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol). It sits between Terraform clients and `registry.terraform.io`, fetching providers on demand and caching them locally and optionally in S3-compatible object storage.

**One-line description:** Pull-through Terraform provider cache registry.

**License:** Apache 2.0

---

## Motivation

- CI/CD agents run in isolated or bandwidth-constrained networks
- `registry.terraform.io` is slow or rate-limited
- GitHub outages cause `terraform init` to fail mid-pipeline for no reason on your end
- Reproducible `terraform init` without manually pinning provider zips
- Durable provider cache that survives pod restarts

---

## Architecture

```
terraform init
      │
      ▼
 terrastrata
      │  cache HIT  → serve from local volume
      │  cache MISS → fetch from registry.terraform.io
      │               ├─ write to local PVC  (fast, ephemeral)
      │               └─ async write to S3   (durable, optional)
      ▼
registry.terraform.io   (only on first request per version)
```

Cache lookup order: **local PVC → S3 (if enabled) → upstream registry**

---

## Tech stack

| Layer | Choice |
|---|---|
| Language | Go 1.26 (stdlib-first: `net/http` ServeMux, `log/slog`) |
| S3 client | AWS SDK v2 (`github.com/aws/aws-sdk-go-v2`) |
| Metrics | Prometheus client (`github.com/prometheus/client_golang`) |
| Container | Multi-stage build, distroless static (nonroot) runtime |
| Deployment | Kubernetes manifests (`deploy/k8s/manifests.yaml`) + Helm chart (`deploy/helm/terrastrata`) |
| Protocol | Terraform Provider Network Mirror Protocol (HTTP/JSON) |

---

## Project structure

```
.
├── cmd/terrastrata/main.go  # Entrypoint: wiring, hardened server, graceful shutdown
├── internal/
│   ├── config/              # Env-driven Config + validation
│   ├── cache/               # Two-layer cache: local FS, S3, Layered composition
│   ├── mirror/              # Protocol: paths, upstream client, translation, handler
│   ├── prewarm/             # Optional startup cache seeding (in-process replay)
│   ├── httpx/               # Middleware: request-id, logging, recovery, bearer auth
│   └── observ/              # slog logger + Prometheus metrics
├── go.mod / go.sum          # Module definition + checksums
├── Dockerfile               # Multi-stage container build (distroless runtime)
├── Makefile                 # build / test / lint / vuln / docker targets
├── deploy/
│   ├── k8s/manifests.yaml   # Namespace, (Secret), PVC, Deployment, Service
│   └── helm/terrastrata/    # Helm chart
├── .github/workflows/
│   ├── ci.yml               # PR: test, lint, govulncheck, image build + Trivy scan
│   └── release.yml          # tags: multi-arch GHCR push, SBOM/provenance, cosign sign
├── README.md                # User-facing documentation
└── CLAUDE.md                # This file
```

---

## Key components

### `internal/config`
All configuration via environment variables. Constructed by `config.FromEnv()`,
which applies defaults and **fails fast** on inconsistent input (e.g. `S3_BUCKET`
set without credentials).

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address |
| `CACHE_DIR` | `/cache` | Local filesystem cache root |
| `CACHE_MAX_BYTES` | _(empty)_ | Local cache size budget (`20GB`/`512Mi`/bytes); LRU eviction over it. Empty/`0` = unbounded |
| `UPSTREAM_BASE` | `https://registry.terraform.io` | Upstream registry |
| `S3_BUCKET` | _(empty)_ | S3 bucket — leave empty to disable S3 |
| `S3_PREFIX` | `tf-mirror` | S3 key prefix |
| `S3_ENDPOINT` | _(empty)_ | Custom S3 endpoint (OVH, MinIO, etc.) |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_ACCESS_KEY` | _(empty)_ | S3 credentials |
| `S3_SECRET_KEY` | _(empty)_ | S3 credentials |
| `AUTH_TOKEN` | _(empty)_ | Optional bearer token on mirror endpoints; empty = auth disabled |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `INDEX_TTL` | `10m` | Versions-index freshness window (Go duration); `0` disables expiry |
| `PREWARM_PROVIDERS` | _(empty)_ | Comma-separated `[host/]ns/type[@version]` to warm at startup; empty disables |
| `PREWARM_PLATFORMS` | `linux_amd64` | Comma-separated `os_arch` for warming zips of `@version` entries |

### `internal/cache`
- `Cache` interface: `Get(ctx, key) (io.ReadCloser, bool, error)` and `Put(ctx, key, io.Reader)` (streaming).
- `Local` — atomic filesystem store (temp-file + fsync + rename); contains all keys within the cache root. Touches mtime on read so it tracks last access.
- `S3` — AWS SDK v2 backend; path-style addressing for custom endpoints (MinIO/OVH).
- `Layered` — composes local → S3: `Get` warms the local layer on an S3 hit; `Put`
  writes local synchronously and S3 asynchronously. A nil durable layer is handled
  transparently (local-only mode).
- `Evictor` — when `CACHE_MAX_BYTES > 0`, a background sweeper (5m) deletes
  least-recently-used files (by mtime) down to ~90% of the budget; skips the
  staging dir and in-progress temp files.

### `internal/mirror`
- `paths.go` — strict validation of every request coordinate (traversal-proof); the cache's first line of defense.
- `upstream.go` — registry-protocol client (`/v1/providers/...`) with transport-level timeouts and bounded response bodies.
- `protocol.go` — translation from registry responses to mirror responses, concurrent (bounded) archives assembly, cache-key helpers.
- `handler.go` — `http.Handler` over a `ServeMux`. Routes:
  - `GET /:hostname/:namespace/:type/index.json` — versions index
  - `GET /:hostname/:namespace/:type/:version.json` — archives index
  - `GET /:hostname/:namespace/:type/:version/download/:platform/:filename` — provider zip
  - Sets `X-Cache: HIT|MISS|STALE`; verifies the registry SHA-256 before caching a zip; treats the cache as best-effort (never a hard dependency).
  - Versions index is revalidated on `INDEX_TTL`; on upstream failure during revalidation it serves the last-known-good copy stale (`freshness.go` holds the envelope helpers).
  - Concurrent cold requests for the same coordinate are coalesced (`golang.org/x/sync/singleflight`): one request fetches from upstream and populates the cache while the rest wait and then serve it, collapsing a thundering herd (e.g. a fleet of CI agents starting at once) into a single upstream fetch. The in-flight fetch runs under a detached context so one client hanging up never aborts the work the others are waiting on.

### `internal/prewarm`
Optional startup cache seeding. Replays mirror requests (`[host/]ns/type[@version]`)
against the handler **in-process** — reusing all validation/caching/checksum logic
with no duplication — discarding zip bodies so nothing is buffered. Best-effort and
backgrounded; never blocks startup or `/health`, and cancels on shutdown.

### `internal/httpx` and `internal/observ`
Cross-cutting HTTP middleware (request-id, structured access logging, panic
recovery, optional constant-time bearer auth) and observability (JSON `slog`
logger + private Prometheus registry on `/metrics`). Metrics: `cache_lookups_total`,
`http_requests_total`, `http_request_duration_seconds`, `versions_index_total`
(freshness outcome: fresh/revalidated/stale/error), `prewarm_total`,
`cache_size_bytes` + `cache_evictions_total`, plus Go/process
collectors. `/health` and `/metrics` are
unauthenticated; mirror routes sit behind optional auth.

---

## Terraform Network Mirror Protocol

terrastrata implements the [network mirror protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol),
which has exactly **two** read endpoints (do not confuse with the richer
*registry* protocol — that distinction is the source of a common bug here):

**1. Versions index** (`GET /:hostname/:namespace/:type/index.json`):
```json
{
  "versions": {
    "3.110.0": {},
    "3.109.0": {}
  }
}
```

**2. Archives index** (`GET /:hostname/:namespace/:type/:version.json`):
```json
{
  "archives": {
    "linux_amd64": {
      "url": "3.110.0/download/linux_amd64/terraform-provider-azurerm_3.110.0_linux_amd64.zip",
      "hashes": ["zh:abc123..."]
    }
  }
}
```

The archive `url` is **relative to the `<version>.json` document's URL**.
terrastrata rewrites it to a self-hosted relative path that encodes os/arch
(`:version/download/:os_:arch/:filename.zip`), so the actual zip is served and
cached by terrastrata at:

**3. Zip** (`GET /:hostname/:namespace/:type/:version/download/:platform/:filename`)

On a cache miss, terrastrata translates these to the upstream **registry**
protocol: `index.json` → `/v1/providers/:ns/:type/versions`, and each archive →
`/v1/providers/:ns/:type/:version/download/:os/:arch` (yielding `download_url`,
`shasum`, `filename`).

---

## Cache directory layout

```
/cache/
└── registry.terraform.io/
    └── hashicorp/
        └── azurerm/
            ├── index.json                  # versions index
            ├── 3.110.0.json                # archives index for 3.110.0
            └── 3.110.0/
                └── download/
                    └── linux_amd64/
                        └── terraform-provider-azurerm_3.110.0_linux_amd64.zip
```

Same structure is mirrored under the configured S3 prefix.

Note: the versions `index.json` is stored as an internal freshness envelope
(`{"fetched_at":..., "body":{...}}`) so its TTL survives copying between cache
layers; only `body` is ever served to clients. Archives `<version>.json` and zips
are stored as raw bytes (immutable per version).

---

## Kubernetes deployment notes

- **Replicas: 1** — PVC is `ReadWriteOnce`. For HA, use `ReadWriteMany` or S3-only mode.
- **Strategy: Recreate** — required for RWO PVC, avoids two pods competing for the volume.
- **PVC size** — 20Gi default. `hashicorp/azurerm` alone can grow to 30–50 GB if all versions are cached.
- **TLS** — terrastrata serves plain HTTP internally. Terminate TLS at Ingress/Gateway.
- **S3 credentials** — stored in a Kubernetes `Secret` (`tf-mirror-s3`).

### Agent `.terraformrc`
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

---

## Known limitations / open TODOs

- Only provider mirror protocol supported — no module registry protocol
- Replicas limited to 1 with RWO PVC (use S3-only / RWX for HA)

---

## Roadmap

- [ ] Support for module registry protocol
- [x] Pre-warm mode: seed cache from a provider list on startup
- [x] Cache TTL / revalidation for index.json (with serve-stale-on-outage)
- [x] Prometheus metrics endpoint
- [x] Helm chart

---

## Target deployment environment

- Kubernetes cluster (existing, internal)
- OVH Object Storage as S3 backend (`s3.de.io.cloud.ovh.net`, region `de`)
- Azure DevOps self-hosted agents as Terraform clients
- Internal network only — no external auth required by default. Optional
  `AUTH_TOKEN` bearer auth exists, but Terraform's `network_mirror` client does
  not send auth headers, so it is only useful behind a header-injecting gateway
  or for non-Terraform consumers; network policy remains the primary boundary.
