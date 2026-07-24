# Local test stack (terrastrata + Prometheus + Grafana)

A self-contained Docker Compose stack for exercising terrastrata under real
workloads and watching its metrics over time. terrastrata is built from this
repo's `Dockerfile`; Prometheus scrapes its `/metrics`; Grafana shows a
pre-provisioned dashboard.

```
 terraform / loadgen ──▶ terrastrata ──▶ registry.terraform.io
                              │ /metrics
                              ▼
                         Prometheus ──▶ Grafana (dashboard)
```

## Prerequisites

- Docker + Docker Compose v2
- **buildx / BuildKit** — the terrastrata image is built from this repo's
  `Dockerfile`, which uses BuildKit cache mounts. If `docker buildx version`
  fails, install it (Arch / CachyOS: `sudo pacman -S docker-buildx`; Debian /
  Ubuntu: `sudo apt install docker-buildx-plugin`). Without it the build fails
  with *"the --mount option requires BuildKit"*.
- Optional: `terraform` (for real `init`/`plan` traffic), `curl` + `python3`
  (for the bundled load generator)

## Quick start

```bash
cd deploy/local
docker compose up --build -d
```

| Service | URL | Notes |
|---|---|---|
| terrastrata (mirror) | http://localhost:8080 | plaintext; `/health`, `/metrics` |
| Grafana | http://localhost:3000 | anonymous admin, dashboard **terrastrata** |
| Prometheus | http://localhost:9090 | scrapes every 5s |

Generate some traffic and watch the dashboard light up:

```bash
./loadgen.sh                 # 120s of versions/archives/zip requests
# or heavier:
DURATION=600 CONCURRENCY=8 ./loadgen.sh hashicorp/azurerm hashicorp/aws
```

Open Grafana → the **terrastrata** dashboard. The first pass is cache MISSes
(fetched from upstream); re-running `loadgen.sh` shows the hit ratio climb.

Tear down (add `-v` to also drop the cached data volumes):

```bash
docker compose down            # keep cache/metrics volumes
docker compose down -v         # wipe everything
```

## Exercising the durable S3 layer (MinIO)

Add the S3 overlay to put a MinIO bucket behind terrastrata (local → S3 →
upstream, async S3 writes, warm-on-S3-hit, durable across restarts):

```bash
docker compose -f docker-compose.yml -f docker-compose.s3.yml up --build -d
```

MinIO console: http://localhost:9001 (`minioadmin` / `minioadmin`). After some
load, restart terrastrata and watch it warm the local cache from S3 instead of
upstream:

```bash
docker compose -f docker-compose.yml -f docker-compose.s3.yml restart terrastrata
```

## Real `terraform init` (TLS overlay)

Terraform refuses a plaintext network mirror, so real terraform clients need
HTTPS. The TLS overlay fronts terrastrata with Caddy at `https://localhost:8443`
using Caddy's internal CA.

```bash
docker compose -f docker-compose.yml -f docker-compose.tls.yml up --build -d
```

Trust Caddy's internal CA once (so terraform accepts the cert):

```bash
# Extract the root CA Caddy generated:
docker compose -f docker-compose.yml -f docker-compose.tls.yml \
  cp caddy:/data/caddy/pki/authorities/local/root.crt ./caddy-root.crt

# Trust it system-wide:
#   Arch / CachyOS:
sudo cp caddy-root.crt /etc/ca-certificates/trust-source/anchors/terrastrata-caddy.crt
sudo update-ca-trust
#   Debian / Ubuntu:
# sudo cp caddy-root.crt /usr/local/share/ca-certificates/terrastrata-caddy.crt
# sudo update-ca-certificates
```

Then run terraform against the mirror:

```bash
cd terraform-example
export TF_CLI_CONFIG_FILE=$PWD/terraformrc
terraform init     # pulls null/random/local through terrastrata
terraform plan
```

Overlays compose: run all three together with
`-f docker-compose.yml -f docker-compose.s3.yml -f docker-compose.tls.yml`.

## What the dashboard shows

Backed by terrastrata's Prometheus metrics:

| Panel | Metric |
|---|---|
| Cache hit ratio (overall + per resource) | `terrastrata_cache_lookups_total{result}` |
| HTTP request rate / status codes | `terrastrata_http_requests_total{route,code}` |
| Request latency p50/p95/p99 | `terrastrata_http_request_duration_seconds_bucket` |
| Upstream fetches/s | misses from `terrastrata_cache_lookups_total` |
| Versions-index outcomes | `terrastrata_versions_index_total{outcome}` |
| Cache size + eviction rate | `terrastrata_cache_size_bytes`, `terrastrata_cache_evicted_bytes_total` |
| Process memory / goroutines | `process_*`, `go_*` |

## Tuning notes

- **Eviction / cache-size panels:** `CACHE_MAX_BYTES` (default `2GB`, set on the
  `terrastrata` service) enables the evictor, which is what publishes
  `terrastrata_cache_size_bytes` and the eviction counters. Lower it (e.g.
  `200MB`) and pull a large provider to watch eviction happen.
- **More traffic:** add bigger providers to `loadgen.sh` (e.g.
  `hashicorp/azurerm`), raise `CONCURRENCY`, or run real `terraform` loops.
- **Scrape resolution:** Prometheus scrapes every 5s (`prometheus/prometheus.yml`)
  and Grafana refreshes every 5s — fine for short sessions; raise the interval
  for multi-day runs.
