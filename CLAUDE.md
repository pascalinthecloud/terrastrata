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
│   ├── mirror/              # Provider protocol: paths, upstream client, translation, handler
│   ├── modules/             # Optional module registry protocol (opt-in, MODULES_ENABLED)
│   ├── pathsafe/            # Traversal-proof path-component validation (shared)
│   ├── freshness/           # Cached-document TTL envelope (shared)
│   ├── prewarm/             # Optional startup cache seeding (in-process replay)
│   ├── httpx/               # Middleware: request-id, logging, recovery, bearer auth
│   └── observ/              # slog logger + Prometheus metrics
├── go.mod / go.sum          # Module definition + checksums
├── Dockerfile               # Multi-stage container build (distroless runtime)
├── Makefile                 # build / test / lint / vuln / docker targets
├── deploy/
│   ├── k8s/manifests.yaml   # Namespace, (Secret), PVC, Deployment, Service
│   └── helm/terrastrata/    # Helm chart
├── docs/                    # Astro + Starlight docs site (canonical docs; Pages)
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
| `UPSTREAM_BASE` | `https://registry.terraform.io` | Upstream registry. Comma-separated list for multi-upstream; each entry a URL or `hostname=url` |
| `MIRROR_HOSTNAME` | _(host of the first `UPSTREAM_BASE` entry)_ | Hostname the mirror serves (the `{hostname}` path segment); other hostnames 404. Overrides the **first** entry only |
| `S3_BUCKET` | _(empty)_ | S3 bucket — leave empty to disable S3 |
| `S3_PREFIX` | `tf-mirror` | S3 key prefix |
| `S3_ENDPOINT` | _(empty)_ | Custom S3 endpoint (OVH, MinIO, etc.) |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_ACCESS_KEY` | _(empty)_ | S3 credentials — set with `S3_SECRET_KEY` or leave both empty for the AWS default credential chain (IRSA etc.) |
| `S3_SECRET_KEY` | _(empty)_ | S3 credentials |
| `AUTH_TOKEN` | _(empty)_ | Optional bearer token on mirror endpoints; empty = auth disabled |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `INDEX_TTL` | `10m` | Versions-index freshness window (Go duration); `0` disables expiry |
| `PREWARM_PROVIDERS` | _(empty)_ | Comma-separated `[host/]ns/type[@version]` to warm at startup; empty disables |
| `PREWARM_PLATFORMS` | `linux_amd64` | Comma-separated `os_arch` for warming zips of `@version` entries |
| `MODULES_ENABLED` | `false` | Serve the module registry protocol (adds `/.well-known/terraform.json` + `/v1/modules/`) |
| `MODULES_UPSTREAM_BASE` | _(value of `UPSTREAM_BASE`)_ | Upstream module registry |

### `internal/cache`
- `Cache` interface: `Get(ctx, key) (io.ReadCloser, bool, error)` and `Put(ctx, key, io.Reader)` (streaming).
- `Local` — atomic filesystem store (temp-file + fsync + rename); contains all keys within the cache root. Touches mtime on read so it tracks last access.
- `S3` — AWS SDK v2 backend; path-style addressing for custom endpoints (MinIO/OVH).
- `Layered` — composes local → S3: `Get` warms the local layer on an S3 hit; `Put`
  writes local synchronously and S3 asynchronously. A nil durable layer is handled
  transparently (local-only mode). With `WithDurableVerifier`, an object warmed
  from S3 is checked before it is served (`internal/mirror/integrity.go` supplies
  the provider-archive verifier: registry digest first, cached `zh:` digest as the
  offline fallback, pass when neither is available). A rejection deletes the local
  copy and is reported as a **miss**, so the handler refetches upstream and heals
  both layers; `cache_integrity_failures_total` records it.
- `Evictor` — when `CACHE_MAX_BYTES > 0`, a background sweeper (5m) deletes
  least-recently-used files (by mtime) down to ~90% of the budget; skips the
  staging dir and in-progress temp files.

### `internal/mirror`
- `paths.go` — strict validation of every request coordinate (traversal-proof); the cache's first line of defense. The handler additionally rejects (404) any `{hostname}` that is not the configured mirror hostname, so foreign hostnames can never alias upstream content under a different cache key.
- `upstream.go` — registry-protocol client (`/v1/providers/...`) with transport-level timeouts and bounded response bodies. Download URLs must be https unless `UPSTREAM_BASE` itself is http (dev/MinIO).
- `protocol.go` — translation from registry responses to mirror responses, concurrent (bounded) archives assembly, cache-key helpers.
- `handler.go` — `http.Handler` over a `ServeMux`. Holds `upstreams map[string]*Upstream`
  keyed by lowercased hostname, so one instance mirrors several registries over a
  shared cache; `provider()` rejects (404) any hostname absent from the map, and
  `upstreamFor(c)` resolves the client from the already-validated coordinates.
  Because every cache key starts with the hostname, two registries publishing the
  same `namespace/type` cannot alias each other. Routes:
  - `GET /:hostname/:namespace/:type/index.json` — versions index
  - `GET /:hostname/:namespace/:type/:version.json` — archives index
  - `GET /:hostname/:namespace/:type/:version/download/:platform/:filename` — provider zip
  - Sets `X-Cache: HIT|MISS|STALE`; verifies the registry SHA-256 before caching a zip; treats the cache as best-effort (never a hard dependency).
  - Versions index is revalidated on `INDEX_TTL`; on upstream failure during revalidation it serves the last-known-good copy stale (`internal/freshness` holds the envelope helpers).
  - Concurrent cold requests for the same coordinate are coalesced (`golang.org/x/sync/singleflight`): one request fetches from upstream and populates the cache while the rest wait and then serve it, collapsing a thundering herd (e.g. a fleet of CI agents starting at once) into a single upstream fetch. The in-flight fetch runs under a detached context so one client hanging up never aborts the work the others are waiting on.

### `internal/modules`
Optional (`MODULES_ENABLED`) module **registry** — not a mirror: Terraform has no
module mirror protocol, so clients address terrastrata directly via
`source = "<host>/<ns>/<name>/<system>"` and discover it through
`/.well-known/terraform.json` (which advertises only `modules.v1`; terrastrata is
a provider *mirror*, not a provider registry).

- `versions` and `<version>/download` are served from cache with the same
  TTL/serve-stale and singleflight machinery as the provider path.
- The one translation is `X-Terraform-Get`: upstream's value is rewritten to a
  **host-relative** terrastrata archive URL (Terraform resolves it against the
  download endpoint, so no external hostname needs configuring).
- **The live registry returns `git::https://github.com/OWNER/REPO?ref=<sha>` for
  every module**, not the https tarball the protocol docs show. Those are mapped
  onto `codeload.github.com/OWNER/REPO/tar.gz/<sha>` so no git client is needed,
  and the tarball's single wrapper directory is stripped on the way through
  (`repack.go`) because **Terraform does not expand the go-getter `//*` subdir
  glob for registry modules** — it records the literal `*` path and fails.
- Anything else (non-GitHub `git::`, `ssh://`, `s3::`, unknown archive type) is
  passed through verbatim with `X-Cache: BYPASS` rather than failing the request.
- **No checksums exist** in this protocol, so archives get only an https-only
  fetch and a 512 MiB cap — weaker than the provider path's SHA-256 verification.
  Because nothing downstream can catch a tampered archive, `repack.go` validates
  both entry names *and* link targets: symlinks/hardlinks resolving outside the
  tree (or absolute) fail the repack, and a hard link's root-relative target is
  stripped of the wrapper directory alongside the names.

Routing constraint: the module and provider route patterns overlap with neither
more specific (`/v1/modules/{ns}/{name}/{sys}/{v}/download` vs
`/{hostname}/{ns}/{type}/{v}/download/{platform}/{filename}`), so registering both
on one `ServeMux` **panics at startup**. Providers stay on `mirrorMux`; modules are
registered on the root mux. The archive endpoint is mounted outside bearer auth
because Terraform sends credentials only to registry endpoints, never to the
`X-Terraform-Get` fetch — so when `AUTH_TOKEN` is set it is authorized by a
15-minute HMAC (keyed on the token, over the coordinates) that the authenticated
download endpoint puts in the URL and `handleArchive` verifies before touching the
cache or upstream (`sign.go`). No token = unsigned URLs and an open endpoint.

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
(labelled by upstream hostname + freshness outcome: fresh/revalidated/coalesced/stale/error), `module_downloads_total`
(cached/bypass/error), `prewarm_total`,
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

Modules, when enabled, live under a separate `_modules/` root that no provider
hostname can collide with:

```
/cache/_modules/claranet/regions/azurerm/
├── versions.json          # version list (freshness envelope)
└── 8.0.6/
    ├── location.json      # resolved upstream source + subdir + archive type
    └── archive            # the module tarball (wrapper dir already stripped)
```

Same structure is mirrored under the configured S3 prefix.

Note: the versions `index.json` is stored as an internal freshness envelope
(`{"fetched_at":..., "body":{...}}`) so its TTL survives copying between cache
layers; only `body` is ever served to clients. Archives `<version>.json` and zips
are stored as raw bytes (immutable per version).

---

## Kubernetes deployment notes

- **Replicas: 1 by default** — the default PVC is `ReadWriteOnce`, so the chart pins one replica with the `Recreate` strategy (avoids two pods competing for the volume).
- **High availability** — run multiple replicas in S3-backed mode: `replicaCount>1` + `persistence.enabled=false` (per-pod `emptyDir` local cache) + `s3.enabled=true` (shared durable layer). The chart then uses a rolling-update `Deployment`, injects a soft pod anti-affinity (overridable via `affinity`/`topologySpreadConstraints`), and renders an optional `PodDisruptionBudget` (`podDisruptionBudget.enabled`). A Helm `fail` guard rejects `replicaCount>1` with a RWO PVC (use S3-backed mode or a RWX `persistence.accessMode`). Coalescing is per-pod, so a cold object is fetched at most once per replica.
- **PVC size** — 20Gi default. `hashicorp/azurerm` alone can grow to 30–50 GB if all versions are cached; cap with `CACHE_MAX_BYTES`.
- **TLS** — terrastrata serves plain HTTP internally. Terminate TLS at Ingress/Gateway.
- **S3 credentials** — stored in a Kubernetes `Secret` (`tf-mirror-s3`).

### Agent `.terraformrc`
```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/" # must be https — Terraform refuses
    include = ["registry.terraform.io/*/*"]  # a plaintext network_mirror URL
  }
  direct {
    exclude = ["registry.terraform.io/*/*"]
  }
}
```
Terraform requires an `https` mirror URL, so agents go through the
TLS-terminating Ingress/Gateway in front of terrastrata, never the plain-HTTP
Service directly.

---

## Known limitations / open TODOs

- Module support is a *registry*, not a mirror: consumers must rewrite each
  module's `source` to point at terrastrata (no transparent interception)
- Module archives are cached unverified — the protocol publishes no checksums
- Only GitHub-hosted `git::` module sources can be cached; others pass through
- Multi-replica HA requires S3-backed mode (or a RWX PVC); the default RWO PVC is single-replica
- The module registry is single-upstream: module requests carry no hostname segment,
  so there is no dimension to multiplex on the way providers do
- The module API path comes from the upstream's `/.well-known/terraform.json`
  (`modules.v1`), resolved lazily once per process with a 5m retry cooldown and a
  `/v1/modules/` fallback — so private registries on non-standard paths work

---

## Roadmap

- [x] Support for module registry protocol (`MODULES_ENABLED`)
- [x] Pre-warm mode: seed cache from a provider list on startup
- [x] Cache TTL / revalidation for index.json (with serve-stale-on-outage)
- [x] Prometheus metrics endpoint
- [x] Helm chart
- [x] Request coalescing (singleflight) for concurrent cold requests
- [x] Multi-replica HA (S3-backed, with PDB + anti-affinity)
- [x] Multi-upstream mirroring (several registries, one cache)

---

## Target deployment environment

- Kubernetes cluster (existing, internal)
- OVH Object Storage as S3 backend (`s3.de.io.cloud.ovh.net`, region `de`)
- Azure DevOps self-hosted agents as Terraform clients
- Internal network only — no external auth required by default. Optional
  `AUTH_TOKEN` bearer auth exists, but Terraform's `network_mirror` client does
  not send auth headers, so it is only useful behind a header-injecting gateway
  or for non-Terraform consumers; network policy remains the primary boundary.
