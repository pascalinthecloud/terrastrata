# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

These changes are on `main` but **not** in the released `v0.1.0` image. Cut a new
tag (e.g. `v0.2.0`) to publish them.

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

[Unreleased]: https://github.com/pascalinthecloud/terrastrata/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/pascalinthecloud/terrastrata/releases/tag/v0.1.0
