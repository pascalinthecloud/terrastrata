# Contributing to terrastrata

Thanks for your interest in improving terrastrata! This document covers the
development workflow and project conventions.

## Prerequisites

- Go **1.26+**
- Optional: `golangci-lint` v2, `govulncheck`, Docker (with buildx), Helm 3,
  `kubectl`

## Development workflow

```bash
make test     # unit tests with the race detector
make lint      # golangci-lint (config in .golangci.yml)
make vuln      # govulncheck
make build     # build ./bin/terrastrata
make run       # run locally against registry.terraform.io (cache in ./cache)
```

Run a local instance and exercise it:

```bash
make run
curl -s localhost:8080/health
curl -s localhost:8080/registry.terraform.io/hashicorp/null/index.json
```

## Project layout

```
cmd/terrastrata     Application entrypoint and server wiring
internal/config     Environment-driven configuration + validation
internal/cache      Two-layer pull-through cache (local FS + optional S3)
internal/mirror     Network mirror protocol: paths, upstream, translation, HTTP
internal/httpx      Middleware (request-id, logging, recovery, bearer auth)
internal/observ     Structured logging and Prometheus metrics
deploy/             Kubernetes manifests and Helm chart
```

## Conventions

- **Standard library first.** New third-party dependencies need a clear
  justification; we keep the dependency surface small for auditability.
- **Tests** accompany behavioral changes. Prefer `httptest` over network calls;
  table-driven tests where natural.
- **Security:** any path segment that becomes a cache key or upstream URL must go
  through the validators in `internal/mirror/paths.go`.
- **Formatting:** `gofmt`/`goimports` clean; `make lint` must pass.
- **Commits:** imperative mood, conventional-commit prefixes (`feat:`, `fix:`,
  `docs:`, …) are appreciated but not mandatory.

## Pull requests

1. Fork and branch from `main`.
2. Ensure `make test lint vuln` all pass.
3. Describe the change and the motivation. Reference any related issue.
