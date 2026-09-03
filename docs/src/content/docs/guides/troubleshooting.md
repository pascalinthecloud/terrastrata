---
title: Troubleshooting
description: The failures people actually hit, and what each one means.
---

## Start with the header

```bash
curl -sI https://tf-mirror.internal/registry.terraform.io/hashicorp/null/index.json \
  | grep -i x-cache
```

`HIT` means cached, `MISS` means it went upstream for this request, `STALE` means
upstream is failing and you are on cached data, `BYPASS` means a module source that
cannot be cached.

## `terraform init` still installs from registry.terraform.io

Your `include` pattern did not match the source address in use. Patterns match the
full address, so `hashicorp/null` in code resolves to
`registry.terraform.io/hashicorp/null` and needs `registry.terraform.io/*/*`.
Mirroring OpenTofu as well means listing `registry.opentofu.org/*/*` too, and the
`direct { exclude = [...] }` block must mirror the same list — otherwise the CLI
quietly falls back to the registry and everything appears to work while nothing is
cached.

## 404 on a provider that definitely exists

The `{hostname}` segment must be one this instance serves. terrastrata refuses any
other hostname rather than proxying it, so a request for
`registry.opentofu.org/...` against an instance configured only for
`registry.terraform.io` is a 404 by design. Check the startup log line — it lists
every `hostname -> upstream` mapping — and see
[mirroring several registries](/terrastrata/guides/multiple-registries/).

## Terraform refuses the mirror URL

`network_mirror` URLs must be `https`. terrastrata serves plain HTTP, so a
TLS-terminating Ingress or Gateway in front of it is mandatory, with a certificate
the agents trust. There is no flag to relax this on the client.

## Module init fails with "Unreadable module subdirectory"

That is the go-getter `//*` glob not being expanded for registry modules, which
terrastrata works around by stripping the tarball's wrapper directory. Seeing it
means the archive you were served was not repacked — most likely a `BYPASS` source
reaching go-getter directly. Check
`terrastrata_module_downloads_total{outcome="bypass"}`.

## Module registry not found at all

Terraform requires a registry hostname containing a dot and discovers the API
through `/.well-known/terraform.json`. `localhost:8443` is rejected as a hostname
before any request is made — use a real name, even a `/etc/hosts` entry.

## 403 on a module archive

With `AUTH_TOKEN` set, archive URLs carry a 15-minute HMAC signature minted by the
download endpoint. A `403` means the signature is missing, tampered with, expired,
or was issued for different coordinates. Fetch the archive URL from the download
endpoint again rather than reusing a stored one, and check the clock skew between
terrastrata and whatever mints requests.

## 401 on every request

`AUTH_TOKEN` is set and the caller is not sending it. Terraform's `network_mirror`
client never sends credentials, so provider traffic behind a bare `AUTH_TOKEN`
cannot work — the token is for a header-injecting gateway or non-Terraform
consumers. Module *registry* endpoints do take a `credentials` block.

## The cache keeps re-downloading

Check `CACHE_MAX_BYTES` against your provider set: an aggressive budget evicts
artifacts you are still using, and `terrastrata_cache_evictions_total` climbing
steadily is the symptom. Also confirm the volume is actually persistent — an
`emptyDir` (which is what `persistence.enabled=false` gives you) is discarded on
every pod restart, which is fine with S3 enabled and expensive without it.

## S3 hits do not seem to happen

Objects land in S3 asynchronously after a local write, so a `Put` that failed shows
up only in the logs (`durable cache put failed`) — the request itself succeeded.
Check credentials, and note that a missing bucket is deliberately **not** treated
as a cache miss: it propagates as an error rather than silently disabling
durability forever.

## Startup fails immediately

Configuration is validated up front, so the log line names the problem: a
half-configured S3 credential pair, credentials without a bucket, an invalid
`INDEX_TTL`, a bad `LOG_LEVEL`, or a `MIRROR_HOSTNAME` that no route could match.
See [configuration](/terrastrata/reference/configuration/).
