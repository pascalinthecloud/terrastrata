---
title: Module registry
description: Caching registry modules, the source rewrite it requires, and the weaker guarantees that come with it.
---

Set `MODULES_ENABLED=true` (chart: `modules.enabled=true`) to also serve
Terraform's
[module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol).

:::caution[This works differently from providers]
Terraform has no *mirror* protocol for modules — only the registry protocol. So
terrastrata cannot transparently intercept module traffic the way it does for
providers. It becomes a registry that clients address directly, which means
**rewriting the `source` of every module you want cached**.
:::

## Client side

```hcl
module "regions" {
  # was: claranet/regions/azurerm
  source  = "tf-mirror.internal/claranet/regions/azurerm"
  version = "8.0.6"
}
```

Terraform discovers the API through
`https://tf-mirror.internal/.well-known/terraform.json`, so the mirror must be
reachable over **https** under a hostname containing a dot — Terraform rejects
`localhost:8443` as a registry hostname outright.

Nothing else changes on the client. Only registry-sourced modules can be cached:
`git::` sources, local paths, and the other
[module source types](https://developer.hashicorp.com/terraform/language/modules/sources)
never touch a registry and are unaffected.

## What actually gets cached

On a download request the upstream registry answers with an `X-Terraform-Get`
header pointing at the module's real source. terrastrata rewrites that to its own
archive endpoint, then fetches, caches, and serves the archive itself.

In practice the public registry returns a `git::` source for **every** module
(`git::https://github.com/OWNER/REPO?ref=<commit>`), despite the protocol docs
showing an https tarball. terrastrata maps GitHub sources onto the equivalent
`codeload.github.com` tarball, so no git client is involved, and the `ref` is
always a commit SHA, so the result is immutable and safe to cache under the module
version forever.

It also strips the single wrapper directory GitHub tarballs add
(`REPO-REF/`), because Terraform does not expand the go-getter `//*` subdir glob
for registry modules — it records the literal `*` path and then fails with
"Unreadable module subdirectory".

## Sources that bypass the cache

A non-GitHub `git::` host, `ssh://`, `s3::`, `hg::`, or an archive whose type
cannot be determined is **passed through unchanged** with `X-Cache: BYPASS`.
`terraform init` still works wherever the client can reach the original source,
but nothing is cached. Watch for it:

```promql
rate(terrastrata_module_downloads_total{outcome="bypass"}[5m])
```

## Pointing at a private registry

`MODULES_UPSTREAM_BASE` can name any registry that speaks the protocol.
terrastrata finds its API the way Terraform does — by reading
`/.well-known/terraform.json` and using the `modules.v1` path it advertises — so
an Artifactory or Nexus repository that serves modules from
`/artifactory/api/terraform/<repo>/v1/modules/` works without extra
configuration. The resolved path is logged once:

```
INFO module API path resolved by service discovery
     upstream=https://nexus.corp path=https://nexus.corp/repository/tf/v1/modules/
```

A registry that serves no discovery document falls back to `/v1/modules/`, which
is what the public registry uses, and the fallback is logged as a warning so a
404-ing private registry is diagnosable rather than mysterious. Discovery is
resolved once per process and retried at most every five minutes while it fails,
so it costs nothing per request. A document advertising a plain-http address is
refused when the upstream itself is https — that would be a downgrade anyone on
the path could ask for.

## With `AUTH_TOKEN` set

Module endpoints behave differently from provider ones, because Terraform *does*
send credentials from a `credentials` block to registry endpoints:

```hcl
credentials "tf-mirror.internal" {
  token = "..."
}
```

The archive download that follows is the exception — go-getter fetches
`X-Terraform-Get` with no `Authorization` header at all. So the archive endpoint
is authorized by its URL instead: the (authenticated) download endpoint mints a
URL carrying a **15-minute HMAC** over the module coordinates, keyed on
`AUTH_TOKEN`, and the archive endpoint answers `403` to anything unsigned,
tampered, or expired — checked before it reads the cache or calls upstream. With
no token configured, archive URLs are unsigned and the endpoint is open, like
every other route.

## The guarantees are weaker than the provider path

:::danger[No checksums exist in this protocol]
The module registry protocol publishes none, so a module archive cannot be
verified against upstream-published bytes the way a provider zip is (checked
against the registry's SHA-256 before caching). Integrity here rests on the
https-only fetch plus a 512 MiB size cap.
:::

What terrastrata does check is the archive's *shape*: while stripping the wrapper
directory it rejects any entry whose name escapes the root, any absolute or
escaping symlink target, and any hard link pointing outside the archive — a
malicious module cannot use terrastrata to hand your agents a tarball that writes
outside its own directory.

If the missing checksums are unacceptable for your threat model, leave
`MODULES_ENABLED` off and let module traffic go direct.

## Known limits

- Module support is a *registry*, not a mirror: consumers must rewrite each
  `source`.
- Only GitHub-hosted `git::` sources can be cached; the rest bypass.
- Single-upstream by design — module requests carry no hostname segment, so there
  is nothing to multiplex the way providers do.
