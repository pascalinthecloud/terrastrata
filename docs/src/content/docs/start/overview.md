---
title: What terrastrata is
description: How the cache works, what it is for, and what it deliberately does not do.
---

terrastrata is a self-hosted proxy implementing the
[Terraform Network Mirror Protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol).
It fetches providers from an upstream registry on demand, caches them locally and
optionally in S3-compatible object storage, and serves everything after the first
request from cache.

It works with **Terraform** and **OpenTofu** — the protocol is identical, and one
instance can [mirror both registries at once](/terrastrata/guides/multiple-registries/).
It can additionally act as a caching [module registry](/terrastrata/guides/module-registry/).

## Reasons to run it

- A GitHub or registry outage stops breaking `terraform init` mid-pipeline for
  reasons that have nothing to do with your change.
- Your CI agents sit in an isolated or bandwidth-constrained network.
- `registry.terraform.io` is slow or rate-limiting you.
- You want a durable provider cache that survives pod restarts.
- You want reproducible installs without hand-pinning provider zips.

## How a request is served

```
terraform init / tofu init
      │
      ▼
 terrastrata
      │  cache HIT  → serve from the local volume
      │  cache MISS → fetch from the upstream registry
      │               ├─ write to the local PVC   (fast, ephemeral)
      │               └─ async write to S3        (durable, survives restarts)
      ▼
registry.terraform.io / registry.opentofu.org   (first request per version only)
```

Lookup order is **local disk → S3 (when enabled) → upstream**. With S3 on, a
restarted pod warms its local volume from the durable layer rather than going
back out to the internet. Every response carries an `X-Cache` header of `HIT`,
`MISS`, `STALE`, or `BYPASS`, which is the fastest way to see what actually
happened — see [troubleshooting](/terrastrata/guides/troubleshooting/).

## What it does beyond caching

- **Request coalescing.** Concurrent requests for the same uncached object are
  collapsed into one upstream fetch; the rest wait and are then served from the
  cache. A fleet of agents starting together costs one download per replica.
- **Checksum verification.** A provider zip is verified against the registry's
  published SHA-256 before it is cached or served. An archive the registry
  publishes no checksum for is refused rather than cached unverified.
- **Serve-stale.** The versions index is revalidated on a TTL (`INDEX_TTL`,
  default 10 minutes) so new releases appear. If the registry is down at
  revalidation time, the last-known-good list is served with `X-Cache: STALE`
  instead of failing the request.
- **Bounded disk use.** With `CACHE_MAX_BYTES` set, a background sweeper evicts
  least-recently-used artifacts down to ~90% of the budget.

## What it is not

- **Not a provider registry.** It serves the *mirror* protocol for providers, so
  clients reach it through a `network_mirror` block rather than by rewriting
  source addresses.
- **Not a transparent module mirror.** Terraform has no mirror protocol for
  modules, so module caching means addressing terrastrata as the registry and
  rewriting each module's `source`. That trade-off is covered in the
  [module registry guide](/terrastrata/guides/module-registry/).
- **Not a TLS terminator.** It serves plain HTTP and expects an Ingress or
  Gateway in front of it — which Terraform requires anyway, since it refuses a
  plaintext `network_mirror` URL.
