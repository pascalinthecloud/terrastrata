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
| Language | Go 1.22 |
| S3 client | AWS SDK v2 (`github.com/aws/aws-sdk-go-v2`) |
| Container | Alpine 3.19, multi-stage build |
| Deployment | Kubernetes (manifests in `k8s/manifests.yaml`) |
| Protocol | Terraform Network Mirror Protocol (HTTP/JSON) |

---

## Project structure

```
.
├── main.go                  # Main application — proxy, cache, mirror protocol
├── go.mod                   # Go module definition
├── go.sum                   # Dependency checksums
├── Dockerfile               # Multi-stage container build
├── k8s/
│   └── manifests.yaml       # Namespace, Secret, PVC, Deployment, Service
├── README.md                # User-facing documentation
└── CLAUDE.md                # This file
```

---

## Key components (main.go)

### `Config`
All configuration via environment variables. Constructed by `configFromEnv()`.

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address |
| `CACHE_DIR` | `/cache` | Local filesystem cache root |
| `UPSTREAM_BASE` | `https://registry.terraform.io` | Upstream registry |
| `S3_BUCKET` | _(empty)_ | S3 bucket — leave empty to disable S3 |
| `S3_PREFIX` | `tf-mirror` | S3 key prefix |
| `S3_ENDPOINT` | _(empty)_ | Custom S3 endpoint (OVH, MinIO, etc.) |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_ACCESS_KEY` | _(empty)_ | S3 credentials |
| `S3_SECRET_KEY` | _(empty)_ | S3 credentials |

### `Cache`
Two-layer cache abstraction:
- `Get(ctx, key)` — checks local filesystem first, then S3 (if enabled), returns `nil` on miss
- `Put(ctx, key, data, contentType)` — writes to local filesystem synchronously, S3 asynchronously in a goroutine
- S3 is entirely optional — if `S3_BUCKET` is empty, `newS3Client()` returns `nil` and all S3 paths are skipped

### `ProxyHandler`
Implements `http.Handler`. Routes:
- `GET /health` — liveness/readiness probe
- `GET /:hostname/:namespace/:type/index.json` — provider versions list
- `GET /:hostname/:namespace/:type/:version/download/:platform/index.json` — per-version download metadata
- `GET /:hostname/:namespace/:type/:version/download/:platform/*.zip` — provider zip binary

Key methods:
- `upstreamURL(mirrorPath)` — maps mirror protocol paths back to registry API endpoints
- `rewriteVersionsIndex(body)` — converts registry `/versions` response to mirror protocol format
- `rewriteDownloadIndex(body, r, path)` — converts registry `/download` response to mirror protocol format, rewrites zip URLs to point at terrastrata itself
- `prefetchZip(downloadURL, cacheKey)` — async goroutine that pre-fetches and caches the zip when a download index is requested

---

## Terraform Network Mirror Protocol

terrastrata implements the [network mirror protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol). Key endpoint shapes:

**Versions index** (`/:hostname/:namespace/:type/index.json`):
```json
{
  "versions": {
    "3.110.0": {},
    "3.109.0": {}
  }
}
```

**Download index** (`/:hostname/:namespace/:type/:version/download/:platform/index.json`):
```json
{
  "archives": {
    "linux_amd64": {
      "url": "https://terrastrata.internal/registry.terraform.io/hashicorp/azurerm/3.110.0/download/linux_amd64/terraform-provider-azurerm_3.110.0_linux_amd64.zip",
      "hashes": ["zh:abc123..."]
    }
  }
}
```

---

## Cache directory layout

```
/cache/
└── registry.terraform.io/
    └── hashicorp/
        └── azurerm/
            ├── index.json
            └── 3.110.0/
                └── download/
                    └── linux_amd64/
                        ├── index.json
                        └── terraform-provider-azurerm_3.110.0_linux_amd64.zip
```

Same structure is mirrored under the configured S3 prefix.

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

- No cache eviction or TTL for `index.json` — versions list can go stale
- No pre-warm on startup — cache is cold until providers are first requested
- No Prometheus metrics endpoint yet
- No Helm chart yet
- Only provider mirror protocol supported — no module registry protocol
- Replicas limited to 1 with RWO PVC

---

## Roadmap

- [ ] Cache eviction / TTL for index.json
- [ ] Pre-warm mode: seed cache from a provider list on startup
- [ ] Prometheus metrics endpoint
- [ ] Helm chart
- [ ] Support for module registry protocol

---

## Target deployment environment

- Kubernetes cluster (existing, internal)
- OVH Object Storage as S3 backend (`s3.de.io.cloud.ovh.net`, region `de`)
- Azure DevOps self-hosted agents as Terraform clients
- Internal network only — no external auth required
