# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- **Module archive URLs are now signed when `AUTH_TOKEN` is set.** The archive
  endpoint has to sit outside the bearer middleware — Terraform attaches registry
  credentials only to registry requests, never to the `X-Terraform-Get` fetch
  that follows — which left it as the one route `AUTH_TOKEN` did not cover: any
  caller who could reach the port could pull cached module archives and drive
  upstream fetches. The (authenticated) download endpoint now mints an archive
  URL carrying a 15-minute HMAC over the module coordinates, keyed on
  `AUTH_TOKEN`, and the archive endpoint answers `403` to anything else —
  checking the signature before it reads the cache or calls upstream. With no
  token configured nothing changes: URLs are unsigned and the endpoint is open.
- **The GitHub-tarball repack now validates link targets.** It checked every
  entry *name* for traversal but passed `symlink`/`hardlink` *targets* through
  untouched, so a module could ship `main.tf -> ../../../../etc/passwd` and
  terrastrata would re-serve it as an archive it had examined — with no checksum
  anywhere on the module path to catch it. Targets escaping the tree, and
  absolute targets, now fail the repack; in-tree links are kept, with a hard
  link's target stripped of the wrapper directory like every other path (it
  previously pointed at a path the repacked archive no longer contained).

## [0.5.1] - 2026-09-03

### Added

- CI runs the real **OpenTofu** CLI against the mirror alongside Terraform, and
  asserts the two registries serve genuinely different bytes for the same
  provider coordinate — the property that makes a shared cache safe.
- Helm chart (0.5.1): Artifact Hub annotations (links, license, images,
  maintainers), so the published chart lists properly on artifacthub.io.

### Changed

- Documentation now states OpenTofu support explicitly. terrastrata always spoke
  the protocol OpenTofu uses, and multi-upstream made mirroring both registries
  from one instance practical, but nothing said so. Verified end to end with
  `tofu init` (OpenTofu 1.12.5), which installs and checksum-verifies a provider
  served from the cache.

## [0.5.0] - 2026-08-04

### Added

- **Multi-upstream mirroring**: one terrastrata can now mirror several registries
  — Terraform, OpenTofu, a private registry — each under its own `{hostname}`
  path segment, sharing a single cache, PVC, and S3 bucket. `UPSTREAM_BASE`
  accepts a comma-separated list where each entry is a URL (the served hostname
  is the URL's host) or `hostname=url` when the two differ:

  ```
  UPSTREAM_BASE=https://registry.terraform.io,https://registry.opentofu.org
  ```

  Cache keys were already namespaced by hostname, so registries publishing the
  same `namespace/type` cannot alias each other. A hostname that is not
  configured still returns 404 rather than being proxied.
- Helm chart (0.5.0): `config.upstreams`, a list of `{hostname?, url}` that
  replaces `config.upstreamBase` when set.

### Changed

- `terrastrata_versions_index_total` gains an `upstream` label, so a stale-serving
  outage can be attributed to a specific registry. Queries that already aggregate
  (`sum by (outcome)`, as the bundled Grafana dashboard does) are unaffected;
  queries selecting raw series will see the extra label.
- `MIRROR_HOSTNAME` now overrides the hostname of the **first** `UPSTREAM_BASE`
  entry. With a single upstream — the only case that existed before — this is
  exactly its previous meaning.

## [0.4.0] - 2026-08-04

### Added

- Optional Terraform **module registry** support (`MODULES_ENABLED=true`, off by
  default), serving `/.well-known/terraform.json` and `/v1/modules/`. Module
  version lists, resolved locations, and archives are cached in the same
  two-layer cache as provider zips, with the same TTL revalidation,
  serve-stale-on-outage, and request coalescing.

  Unlike providers this is a *registry*, not a mirror — Terraform has no module
  mirror protocol — so each module's `source` must be rewritten to point at
  terrastrata (`source = "tf-mirror.internal/<ns>/<name>/<system>"`).

  Two behaviors worth knowing: the public registry hands out `git::` sources for
  every module (not the https tarball its protocol docs show), which terrastrata
  maps onto GitHub's codeload tarball so no git client is needed; and module
  archives are cached **unverified**, because the protocol publishes no
  checksums. Sources that cannot be fetched (non-GitHub `git::`, `ssh://`,
  `s3::`) are passed through with `X-Cache: BYPASS` instead of failing.
- `terrastrata_module_downloads_total{outcome}` metric (`cached`/`bypass`/`error`).
  A rising `bypass` rate means modules are resolving but not being cached.
- Helm chart (0.4.0): `modules.enabled` and `modules.upstreamBase`.

### Changed

- Path-component validation moved to `internal/pathsafe` and the versions-index
  freshness envelope to `internal/freshness`, both now shared by the provider and
  module paths. No behavior change.

## [0.3.1] - 2026-08-03

### Added

- Helm chart (0.3.1): optional `ServiceMonitor` (`serviceMonitor.enabled=true`)
  for Prometheus-operator and VictoriaMetrics-operator stacks, which ignore the
  `prometheus.io/*` pod annotations. Set `serviceMonitor.labels` if your
  Prometheus selects monitors by label.
- End-to-end CI test: the real Terraform CLI runs `terraform init` through the
  mirror (behind a TLS proxy) and the second install must be served entirely
  from cache.
- The Helm chart is published to GHCR as a cosign-signed OCI artifact on each
  release: `helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata`.

### Fixed

- Docs: the agent `.terraformrc` examples showed a plain-http mirror URL;
  Terraform requires `https` for `network_mirror`, so agents must go through
  the TLS-terminating Ingress/Gateway.

## [0.3.0] - 2026-07-24

### Fixed

- Layered cache: an S3 hit is served even when warming it into the local layer
  fails (degraded disk); previously the request errored and fell back upstream.
- Zip checksum verification accepts uppercase hex digests and rejects malformed
  (non-SHA-256) digests before downloading.
- Panicking requests now appear in metrics and the access log with status 500;
  previously they were only visible to the client.

### Changed

- **Breaking:** requests whose `{hostname}` path segment does not match the
  mirror's hostname now return 404 instead of being proxied to the configured
  upstream. The hostname defaults to the host of `UPSTREAM_BASE`; set
  `MIRROR_HOSTNAME` when clients address the mirror's providers by a different
  name. Objects previously cached under mismatched hostnames become unreachable
  (harmless — eviction ages them out, or delete them manually).
- Provider download URLs published by the registry must be `https` unless
  `UPSTREAM_BASE` itself is `http` (local/dev setups).
- `terrastrata_versions_index_total` gained the outcome label value
  `"coalesced"`: requests that waited on a concurrent revalidation no longer
  count as `"revalidated"`, so that series now tracks real upstream fetches.
  Update dashboards that sum specific outcome values.
- Helm chart: `config.cacheMaxBytes` now defaults to `18GB` (matching the
  default 20Gi PVC) so a default install cannot fill its volume; set it to `""`
  for the previous unbounded behavior.
- CI: all GitHub Actions pinned by commit SHA, chart/manifest validation job
  (helm lint + kubeconform), and a weekly Trivy re-scan of the published image.

### Added

- AWS default credential chain for S3: leave `S3_ACCESS_KEY`/`S3_SECRET_KEY`
  empty to use IRSA, instance profiles, or environment credentials. The Helm
  chart skips the credentials Secret accordingly and supports
  `serviceAccount.annotations` for IRSA role binding.
- `Content-Length` on cache-served responses.

## [0.2.0] - 2026-06-11

### Added

- Request coalescing (singleflight): concurrent cold requests for the same
  coordinate collapse into a single upstream fetch, preventing a thundering herd.
- Size-bounded LRU eviction for the local cache via `CACHE_MAX_BYTES`
  (e.g. `18GB`); empty/`0` disables it.
- Prometheus metrics for versions-index freshness outcomes and pre-warm results.
- Multi-replica high availability in the Helm chart: S3-backed mode with an
  optional `PodDisruptionBudget`, default pod anti-affinity, a
  `topologySpreadConstraints` passthrough, and a render-time guard against the
  unsafe `ReadWriteOnce` + multi-replica combination.

### Changed

- Bumped all pinned GitHub Actions to their Node 24 runtime majors.
- Cache read path skips the mtime touch when eviction is disabled; the evictor
  uses a cheaper two-pass sweep.
- Helm chart version `0.1.0` → `0.2.0`.

## [0.1.0] - 2026-06-10

Initial release.

### Added

- Terraform provider network mirror protocol: versions index and archives index
  endpoints, with translation to the upstream registry protocol on cache miss.
- Two-layer pull-through cache: atomic local filesystem store and an optional
  S3-compatible durable layer (`Layered` composition with async S3 writes and
  local warm-on-S3-hit).
- SHA-256 verification of provider archives against the registry-published
  checksum before caching or serving.
- Versions-index TTL revalidation (`INDEX_TTL`) with serve-last-known-good on
  upstream failure (`X-Cache: STALE`).
- Optional startup pre-warming (`PREWARM_PROVIDERS` / `PREWARM_PLATFORMS`) via
  in-process request replay.
- Prometheus `/metrics` (cache hit/miss and HTTP request counts/latency),
  `/health` endpoint, structured JSON access logs, and per-request `X-Request-Id`.
- Optional constant-time bearer-token auth (`AUTH_TOKEN`) on mirror endpoints.
- Hardened HTTP server (read/write/idle timeouts), strict path validation
  (traversal-proof), and graceful shutdown.
- Distroless non-root container image; Kubernetes manifests and a Helm chart.
- CI (test, lint, govulncheck, Trivy scan) and a release pipeline publishing a
  signed (cosign keyless), multi-arch image with SBOM and provenance to GHCR.

[Unreleased]: https://github.com/pascalinthecloud/terrastrata/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/pascalinthecloud/terrastrata/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/pascalinthecloud/terrastrata/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/pascalinthecloud/terrastrata/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/pascalinthecloud/terrastrata/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/pascalinthecloud/terrastrata/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/pascalinthecloud/terrastrata/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/pascalinthecloud/terrastrata/releases/tag/v0.1.0
