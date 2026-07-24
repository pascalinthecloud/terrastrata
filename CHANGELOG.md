# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/pascalinthecloud/terrastrata/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/pascalinthecloud/terrastrata/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/pascalinthecloud/terrastrata/releases/tag/v0.1.0
