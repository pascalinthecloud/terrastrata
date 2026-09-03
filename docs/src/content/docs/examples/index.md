---
title: Examples
description: Working configurations for CI agents, local development, and pre-warming.
---

## Azure DevOps self-hosted agents

Write the CLI config in the pipeline rather than baking it into the agent image,
so the mirror hostname is a pipeline variable rather than a rebuild:

```yaml
steps:
  - script: |
      cat > "$(Agent.TempDirectory)/terraform.tfrc" <<'EOF'
      provider_installation {
        network_mirror {
          url     = "https://tf-mirror.internal/"
          include = ["registry.terraform.io/*/*"]
        }
        direct {
          exclude = ["registry.terraform.io/*/*"]
        }
      }
      EOF
      echo "##vso[task.setvariable variable=TF_CLI_CONFIG_FILE]$(Agent.TempDirectory)/terraform.tfrc"
    displayName: Point Terraform at the mirror

  - script: terraform init -input=false
    displayName: terraform init
```

If your agents run in the same cluster as terrastrata, the Service name works
directly — but Terraform still requires https, so go through the Ingress or run a
TLS sidecar.

## GitHub Actions runners

```yaml
- name: Configure the provider mirror
  run: |
    cat > "$RUNNER_TEMP/terraform.tfrc" <<'EOF'
    provider_installation {
      network_mirror {
        url     = "https://tf-mirror.internal/"
        include = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
      }
      direct {
        exclude = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
      }
    }
    EOF
    echo "TF_CLI_CONFIG_FILE=$RUNNER_TEMP/terraform.tfrc" >> "$GITHUB_ENV"
```

## Local development with MinIO

Exercise the S3 path without an account. terrastrata allows a plain-http upstream
and plain-http download URLs **only** when `UPSTREAM_BASE` is itself http, which is
what makes a local setup possible without weakening the production default:

```bash
docker run -d --name minio -p 9000:9000 \
  -e MINIO_ROOT_USER=minio -e MINIO_ROOT_PASSWORD=minio123 \
  minio/minio server /data

docker run -d --name terrastrata -p 8080:8080 \
  -e S3_BUCKET=tf-mirror \
  -e S3_ENDPOINT=http://host.docker.internal:9000 \
  -e S3_ACCESS_KEY=minio -e S3_SECRET_KEY=minio123 \
  -e LOG_LEVEL=debug \
  ghcr.io/pascalinthecloud/terrastrata:0.5.2
```

`deploy/local/` has a fuller version of this with Prometheus, Grafana, and a TLS
front for real clients.

## Pre-warming the providers your pipelines block on

```yaml
prewarm:
  providers:
    - hashicorp/azurerm@3.110.0
    - hashicorp/null@3.2.2
    - registry.opentofu.org/hashicorp/null@3.2.2
  platforms:
    - linux_amd64
    - linux_arm64
```

A bare `namespace/type` warms only the versions index — enough to make `init` fast
without downloading zips for versions nobody asked for. Adding `@version` warms
that version's archives index and its zips for each listed platform. Warming runs
in the background and never blocks `/health`, so a slow registry delays the warm
cache rather than the rollout.

## Bounding disk on a small volume

```yaml
persistence:
  size: 10Gi
config:
  cacheMaxBytes: "8GB"
```

Keep the budget below the volume so eviction happens before the disk fills. The
sweeper runs every 5 minutes and evicts least-recently-used files to ~90% of the
budget, using read-time mtime updates as the LRU signal.

## High availability

See the [high availability guide](/terrastrata/guides/high-availability/) for the
S3-backed multi-replica configuration, which is the one shape where more than one
replica is supported.
