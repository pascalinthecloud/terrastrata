---
title: Cache layout
description: What terrastrata writes to disk and to S3, and why the shape matters.
---

Artifacts are stored in the network mirror protocol's own directory layout, which
makes the cache directly inspectable: a path tells you exactly which coordinate it
belongs to.

## Providers

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

Every key starts with the registry hostname. That is load-bearing rather than
cosmetic: `registry.terraform.io` and `registry.opentofu.org` ship **different
bytes for the same provider coordinate**, so a shared key would serve one
registry's provider to a client asking for the other's.

The same structure is mirrored under your configured S3 prefix.

## Modules

Modules live under a `_modules/` root. No provider hostname can collide with it,
because a validated hostname must start with an alphanumeric character:

```
cache/
└── _modules/
    └── claranet/
        └── regions/
            └── azurerm/
                ├── versions.json        # version list
                └── 8.0.6/
                    ├── location.json    # resolved upstream source + archive type
                    └── archive          # the module tarball, wrapper dir stripped
```

## Two storage formats

Most objects are stored as the exact bytes served. The **versions index** is the
exception: it is wrapped in a freshness envelope,

```json
{"fetched_at": "2026-09-03T12:00:00Z", "body": {"versions": {"3.2.2": {}}}}
```

so its TTL is evaluated against the original upstream fetch no matter how many
times the object is copied between the local and S3 layers. Only `body` is ever
sent to a client. Archives indexes and zips are immutable per version and need no
envelope.

## Writes are atomic

A cache write streams to a temp file in the destination directory, fsyncs it,
renames it into place, then fsyncs the directory. A reader therefore never sees a
partial file, and a node crash cannot leave a truncated archive that would later
be served as a `HIT`.

## Staging

Provider zips are verified before they are cached, which needs somewhere to put
the bytes while the SHA-256 is computed: `CACHE_DIR/.staging`. The evictor skips
that directory and any in-progress `.tmp-*` file, so a sweep never deletes a
download in flight.
