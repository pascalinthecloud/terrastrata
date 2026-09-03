---
title: Observability
description: Health, metrics, logs, and a local stack for watching them under load.
---

## Endpoints

- `GET /health` — liveness and readiness. Dependency-free by design: the cache
  directory must exist for the process to have started, and upstreams are reached
  lazily, so a plain OK is the honest liveness answer. Always unauthenticated.
- `GET /metrics` — Prometheus exposition. Always unauthenticated.

Full series list: [metrics reference](/terrastrata/reference/metrics/).

## The header worth reading first

Every response carries `X-Cache`:

| Value | Meaning |
|---|---|
| `HIT` | Served from cache |
| `MISS` | Fetched upstream to satisfy this request, then served |
| `STALE` | Upstream failed during revalidation; last-known-good served |
| `BYPASS` | A module source terrastrata cannot cache, passed through |

`curl -sI` against a coordinate is usually faster than a metrics query when you
are trying to answer "is this thing actually caching".

## Logs

Structured JSON on stdout, one line per request, including method, path, status,
bytes, duration, and a `request_id` that is echoed back as `X-Request-Id`. An
inbound `X-Request-Id` is reused, so a trace id from your gateway carries through.

`LOG_LEVEL=debug` adds cache-write and durable-upload detail; `info` is one line
per request.

## Scraping

```bash
--set serviceMonitor.enabled=true
# --set serviceMonitor.labels.release=kube-prometheus-stack   # if selected by label
```

The chart also sets `prometheus.io/*` pod annotations, which only apply to classic
annotation-based scrape configs.

## Alerts worth having

```promql
# An upstream registry is failing and you are running on cached data.
sum by (upstream) (rate(terrastrata_versions_index_total{outcome="stale"}[10m])) > 0

# Modules are resolving but not being cached.
rate(terrastrata_module_downloads_total{outcome="bypass"}[15m]) > 0

# The cache is close to its budget and evicting constantly.
rate(terrastrata_cache_evictions_total[30m]) > 0
```

The first two are the ones that matter: both describe a system that looks healthy
from the client's side while quietly not doing its job.

## A local stack

`deploy/local/` runs terrastrata with Prometheus and Grafana (with a provisioned
dashboard), optional MinIO for the S3 layer, and a TLS front so real `terraform`
and `tofu` clients can be pointed at it. It exists for exercising the cache under
load and watching these series move before you rely on them in production.
