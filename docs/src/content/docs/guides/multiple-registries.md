---
title: Mirroring several registries
description: Serve Terraform, OpenTofu, and private registries from one deployment and one cache.
---

One terrastrata can mirror several registries at once. Each is addressed under its
own `{hostname}` path segment and they share a single cache, PVC, and S3 bucket.

## Server side

List the upstreams in `UPSTREAM_BASE`:

```bash
# hostnames derived from each URL's host
UPSTREAM_BASE=https://registry.terraform.io,https://registry.opentofu.org

# explicit hostname where it differs from the upstream URL
UPSTREAM_BASE=https://registry.terraform.io,registry.corp.example=https://nexus.corp/repo/tf
```

With the chart:

```yaml
config:
  upstreams:
    - url: https://registry.terraform.io
    - url: https://registry.opentofu.org
    - hostname: registry.corp.example
      url: https://nexus.corp/repo/tf
```

The `hostname=url` form exists for private registries whose served name differs
from the URL they live at — a Nexus or Artifactory repository on a shared host,
for instance.

## Client side

`include` patterns match per source address, so list every registry in one
`network_mirror` block:

```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/"
    include = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
  }
}
```

## Why the cache cannot alias

Every cache key begins with the registry hostname, so two registries publishing
the same `namespace/type` never see each other's artifacts. This matters more than
it sounds: `registry.terraform.io` and `registry.opentofu.org` serve **different
bytes for the same provider coordinate** — verified on `hashicorp/null` 3.2.2 —
and CI asserts they still differ, because the day they stop differing is the day a
shared key would go unnoticed.

A hostname that is not configured returns **404** rather than being proxied to
some default upstream. Silently forwarding it would cache a foreign registry's
content under a hostname you do trust.

## Pre-warming across upstreams

Write the host into the entry:

```bash
PREWARM_PROVIDERS=hashicorp/null@3.2.2,registry.opentofu.org/hashicorp/null@3.2.2
```

An entry without a host uses `MIRROR_HOSTNAME` (the first upstream).

## Telling them apart in metrics

`terrastrata_versions_index_total` carries an `upstream` label, so a stale-serving
registry is identifiable during an incident:

```promql
sum by (upstream) (rate(terrastrata_versions_index_total{outcome="stale"}[5m]))
```

## Modules stay single-upstream

Module requests carry no hostname segment — clients address terrastrata as the
registry itself — so there is nothing to multiplex on.
`MODULES_UPSTREAM_BASE` picks the one module upstream and defaults to the first
`UPSTREAM_BASE` entry.
