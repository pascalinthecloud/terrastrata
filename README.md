# terrastrata

> Pull-through provider cache registry for Terraform and OpenTofu

[![CI](https://github.com/pascalinthecloud/terrastrata/actions/workflows/ci.yml/badge.svg)](https://github.com/pascalinthecloud/terrastrata/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pascalinthecloud/terrastrata?logo=github)](https://github.com/pascalinthecloud/terrastrata/releases/latest)
[![Docs](https://img.shields.io/badge/docs-pascalinthecloud.github.io-blue)](https://pascalinthecloud.github.io/terrastrata/)
[![Go](https://img.shields.io/github/go-mod/go-version/pascalinthecloud/terrastrata)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**terrastrata** is a lightweight self-hosted proxy that implements the [Terraform Network Mirror Protocol](https://developer.hashicorp.com/terraform/internals/provider-network-mirror-protocol). It fetches providers from the upstream registry on demand, caches them locally and in S3-compatible object storage, and serves subsequent requests entirely from cache — no repeated upstream calls, no internet dependency after first use.

Works with **Terraform** and **OpenTofu** — the network mirror protocol is the same, and one instance can mirror both registries at once. It can also act as a caching module registry.

📖 **[Documentation](https://pascalinthecloud.github.io/terrastrata/)** — installation, configuration reference, guides, and examples.

---

## Why

- You are tired of GitHub outages causing terraform init to fail mid-pipeline for no reason on your end
- Your CI/CD agents run in an isolated or bandwidth-constrained network
- `registry.terraform.io` is slow, rate-limited, or simply unreachable
- You want reproducible `terraform init` / `tofu init` without pinning provider zips manually
- You need a durable provider cache that survives pod restarts

## How it works

```
terraform init / tofu init
      │
      ▼
 terrastrata
      │  cache HIT  → serve from local volume
      │  cache MISS → fetch from the upstream registry
      │               ├─ write to local PVC  (fast, ephemeral)
      │               └─ async write to S3   (durable, survives restarts)
      ▼
registry.terraform.io / registry.opentofu.org   (only on first request per version)
```

Cache lookup order: **local PVC → S3 (if enabled) → upstream registry**. When S3 is
enabled it warms the local volume on pod restart, so nothing is re-fetched from the
internet. Concurrent requests for the same uncached provider are coalesced into a
single upstream fetch, and every archive is verified against the registry-published
SHA-256 before it is cached.

## Quick start

Install with Helm:

```bash
helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata \
  --version 0.5.1 \
  --namespace tf-mirror --create-namespace
```

Point your agents at it via `~/.terraformrc` (or `~/.tofurc`):

```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/" # your Ingress/Gateway hostname
    include = ["registry.terraform.io/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*"]
  }
}
```

Then run `terraform init` as normal. The mirror URL must be **https** — Terraform
refuses a plaintext `network_mirror` URL — so terminate TLS at your Ingress or
Gateway.

Full walkthrough: **[Install](https://pascalinthecloud.github.io/terrastrata/start/install/)**
and **[Point your clients at it](https://pascalinthecloud.github.io/terrastrata/start/clients/)**.

## Documentation

| | |
|---|---|
| [What terrastrata is](https://pascalinthecloud.github.io/terrastrata/start/overview/) | how the cache works, and what it deliberately does not do |
| [Configuration](https://pascalinthecloud.github.io/terrastrata/reference/configuration/) | every environment variable, with defaults |
| [Helm values](https://pascalinthecloud.github.io/terrastrata/reference/helm-values/) | every chart value, and the combinations it refuses |
| [Mirroring several registries](https://pascalinthecloud.github.io/terrastrata/guides/multiple-registries/) | Terraform, OpenTofu, and private registries from one deployment |
| [Module registry](https://pascalinthecloud.github.io/terrastrata/guides/module-registry/) | caching modules, and the weaker guarantees involved |
| [High availability](https://pascalinthecloud.github.io/terrastrata/guides/high-availability/) | multiple replicas in S3-backed mode |
| [Observability](https://pascalinthecloud.github.io/terrastrata/guides/observability/) | health, metrics, logs, alerts worth having |
| [Metrics](https://pascalinthecloud.github.io/terrastrata/reference/metrics/) | every Prometheus series and label |
| [Supply chain](https://pascalinthecloud.github.io/terrastrata/guides/supply-chain/) | signature verification, SBOMs, dependency scanning |
| [Troubleshooting](https://pascalinthecloud.github.io/terrastrata/guides/troubleshooting/) | the failures people actually hit |
| [Examples](https://pascalinthecloud.github.io/terrastrata/examples/) | Azure DevOps, GitHub Actions, MinIO, pre-warming |

## Container images

```
ghcr.io/pascalinthecloud/terrastrata:0.5.1     # exact version
ghcr.io/pascalinthecloud/terrastrata:0.5       # major.minor
ghcr.io/pascalinthecloud/terrastrata:sha-<sha> # by commit
```

Multi-arch (`linux/amd64`, `linux/arm64`) on a distroless runtime, signed with
cosign and shipped with SBOMs and build provenance. Verification commands and the
bill-of-materials files are documented under
[supply chain](https://pascalinthecloud.github.io/terrastrata/guides/supply-chain/).

## Building

```bash
make build   # ./bin/terrastrata
make test    # test suite with the race detector
make lint
docker build -t your-registry/terrastrata:latest .
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow and conventions, including how to run the documentation site
locally. By participating you agree to the
[Code of Conduct](CODE_OF_CONDUCT.md). Security issues should be reported privately
per the [Security Policy](SECURITY.md). Notable changes are recorded in the
[Changelog](CHANGELOG.md).

## License

Apache 2.0
