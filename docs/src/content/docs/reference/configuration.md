---
title: Configuration
description: Every environment variable terrastrata reads, with defaults.
---

All configuration comes from environment variables, validated at startup:
an inconsistent combination fails immediately rather than at the first request.

## Server

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address and port to listen on |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. Anything else is an error, not a silent fallback |
| `AUTH_TOKEN` | _(empty)_ | Bearer token required on mirror and registry endpoints. Empty disables auth. `/health` and `/metrics` are always open |

## Cache

| Variable | Default | Description |
|---|---|---|
| `CACHE_DIR` | `/cache` | Local filesystem cache root. Must be writable — the container root filesystem is read-only |
| `CACHE_MAX_BYTES` | _(empty)_ | Size budget for the local cache: `20GB`, `512Mi`, or a raw byte count. Over budget, least-recently-used files are evicted to ~90% of it. Empty or `0` means unbounded |
| `INDEX_TTL` | `10m` | How long a cached **versions index** is served before revalidation (Go duration). `0` disables expiry. Archives and zips are immutable per version and never expire |

## Upstreams

| Variable | Default | Description |
|---|---|---|
| `UPSTREAM_BASE` | `https://registry.terraform.io` | Upstream registry base URL. Accepts a comma-separated list to [mirror several registries](/terrastrata/guides/multiple-registries/); each entry is a URL, or `hostname=url` when the served hostname differs from the upstream's |
| `MIRROR_HOSTNAME` | _(host of the first `UPSTREAM_BASE` entry)_ | The `{hostname}` path segment this mirror answers for. Requests for any other hostname get a 404. With several upstreams it overrides the **first** entry only — use `hostname=url` for the rest |

A hostname that is not configured is refused rather than proxied. That is
deliberate: forwarding an unknown hostname would cache one registry's content
under another's keys.

## S3 (durable layer)

| Variable | Default | Description |
|---|---|---|
| `S3_BUCKET` | _(empty)_ | Bucket name. Empty disables S3 entirely (local disk only) |
| `S3_PREFIX` | `tf-mirror` | Key prefix inside the bucket |
| `S3_ENDPOINT` | _(empty)_ | Custom endpoint for OVH, MinIO, and other S3-compatible stores. Setting it also switches on path-style addressing |
| `S3_REGION` | `us-east-1` | Region |
| `S3_ACCESS_KEY` | _(empty)_ | Access key. Set **together with** `S3_SECRET_KEY`, or leave both empty to use the AWS default credential chain (IRSA, instance profile, environment) |
| `S3_SECRET_KEY` | _(empty)_ | Secret key |

A half-configured credential pair is rejected at startup instead of failing later
inside an asynchronous upload. Credentials or an endpoint without a bucket is
also an error — it almost always means someone expected S3 to be on.

## Pre-warming

| Variable | Default | Description |
|---|---|---|
| `PREWARM_PROVIDERS` | _(empty)_ | Comma-separated providers to warm at startup, each `[host/]namespace/type[@version]`. A bare provider warms only its versions index; with `@version` it also warms that version's archives index and zips. Empty disables pre-warming |
| `PREWARM_PLATFORMS` | `linux_amd64` | Comma-separated `os_arch` list to warm zips for. Applies only to `@version` entries |

Pre-warming is best-effort and runs in the background: it never blocks startup or
`/health`, and failures are logged rather than fatal. Watch
`terrastrata_prewarm_total` to see how it went.

## Modules

| Variable | Default | Description |
|---|---|---|
| `MODULES_ENABLED` | `false` | Serve the [module registry protocol](/terrastrata/guides/module-registry/), adding `/.well-known/terraform.json` and `/v1/modules/` |
| `MODULES_UPSTREAM_BASE` | _(first `UPSTREAM_BASE` entry)_ | Upstream module registry. Only needed when modules come from a different host than providers. Its API path is discovered from `/.well-known/terraform.json`, so a private registry serving modules from a non-standard path needs no extra configuration |

## An OVH Object Storage example

```yaml
- name: S3_BUCKET
  value: "tf-mirror"
- name: S3_ENDPOINT
  value: "https://s3.de.io.cloud.ovh.net"
- name: S3_REGION
  value: "de"
```
