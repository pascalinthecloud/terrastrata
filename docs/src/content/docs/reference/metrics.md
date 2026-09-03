---
title: Metrics
description: The Prometheus series terrastrata exposes, and which ones deserve alerts.
---

`GET /metrics` serves a private Prometheus registry (always unauthenticated),
including Go runtime and process collectors alongside the series below.

## Cache and traffic

| Series | Labels | Meaning |
|---|---|---|
| `terrastrata_cache_lookups_total` | `resource`, `result` | Cache lookups by resource kind (`versions`, `archives`, `zip`, `module_versions`, `module_location`, `module_archive`) and `hit`/`miss` |
| `terrastrata_http_requests_total` | `route`, `code` | Requests by matched route pattern and status code |
| `terrastrata_http_request_duration_seconds` | `route` | Latency histogram. Buckets run to 120s because a cold provider zip from upstream can legitimately take tens of seconds |

The `route` label is the matched `ServeMux` pattern rather than the raw path, so
cardinality stays bounded no matter what clients request.

## Freshness

| Series | Labels | Meaning |
|---|---|---|
| `terrastrata_versions_index_total` | `upstream`, `outcome` | How a versions-index request was satisfied, per mirrored registry |

Outcomes: `fresh` (served inside the TTL), `revalidated` (this request refetched
upstream), `coalesced` (waited on a concurrent refetch), `stale` (last-known-good
served after an upstream failure), `error` (upstream failed and nothing was
cached to fall back on).

:::tip[Worth an alert]
A rising `outcome="stale"` rate means an upstream registry is failing and you are
running on cached data. Nothing is broken for your users yet — which is exactly
why it needs to page someone rather than go unnoticed. The `upstream` label tells
you which registry.
:::

## Modules

| Series | Labels | Meaning |
|---|---|---|
| `terrastrata_module_downloads_total` | `outcome` | `cached` (served from terrastrata's own archive), `bypass` (a source it cannot cache, passed through), `error` |

:::tip[Worth an alert]
A rising `outcome="bypass"` rate means modules are resolving but **not** being
cached — clients are going to the original source. See
[module registry](/terrastrata/guides/module-registry/) for which sources bypass.
:::

## Housekeeping

| Series | Meaning |
|---|---|
| `terrastrata_prewarm_total{resource,result}` | Startup pre-warm successes and failures |
| `terrastrata_cache_size_bytes` | Local cache size, measured on each evictor sweep (every 5 minutes) |
| `terrastrata_cache_evictions_total` | Files evicted to stay inside the budget |
| `terrastrata_cache_evicted_bytes_total` | Bytes evicted |
| `terrastrata_cache_integrity_failures_total` | Archives from the durable layer rejected because their digest did not match the published one |

`cache_size_bytes` only updates when eviction is enabled (`CACHE_MAX_BYTES`),
since that is what schedules the sweep that measures it.

:::tip[Worth an alert]
`cache_integrity_failures_total` should be flat at zero. Anything else means
shared storage held a provider archive that does not match the digest the registry
publishes for it — corruption at best, tampering at worst. The request itself
succeeded (the archive was refetched and the cache repaired), so nothing else will
tell you.
:::

## Scraping

With an operator-based stack (kube-prometheus-stack, VictoriaMetrics operator),
enable the chart's ServiceMonitor:

```bash
--set serviceMonitor.enabled=true
# plus serviceMonitor.labels if your Prometheus selects monitors by label
```

The `prometheus.io/*` pod annotations the chart also sets only apply to classic
annotation-based scrape configs.
