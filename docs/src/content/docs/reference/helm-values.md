---
title: Helm values
description: The chart's values, what they do, and the combinations it refuses.
---

Install from the OCI registry (`oci://ghcr.io/pascalinthecloud/charts/terrastrata`)
or from a checkout (`deploy/helm/terrastrata`). Values below are the chart
defaults.

## Image and scheduling

| Value | Default | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/pascalinthecloud/terrastrata` | |
| `image.tag` | `""` | Falls back to the chart's `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `replicaCount` | `1` | More than 1 requires S3-backed mode or an RWX volume — see [high availability](/terrastrata/guides/high-availability/) |
| `resources` | 50m/64Mi requests, 500m/256Mi limits | The process is I/O bound, not CPU bound |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | unset | With `replicaCount > 1` the chart injects a soft pod anti-affinity, which setting `affinity` overrides |

## Application config

| Value | Default | Maps to |
|---|---|---|
| `config.listenAddr` | `":8080"` | `LISTEN_ADDR` |
| `config.upstreamBase` | `https://registry.terraform.io` | `UPSTREAM_BASE` |
| `config.upstreams` | `[]` | A list of `{hostname, url}` entries, rendered into a multi-upstream `UPSTREAM_BASE`. Takes precedence over `upstreamBase` |
| `config.logLevel` | `"info"` | `LOG_LEVEL` |
| `config.indexTTL` | `"10m"` | `INDEX_TTL` |
| `config.cacheMaxBytes` | `"18GB"` | `CACHE_MAX_BYTES` — deliberately below the 20Gi PVC so eviction happens before the volume fills |
| `modules.enabled` | `false` | `MODULES_ENABLED` |
| `modules.upstreamBase` | `""` | `MODULES_UPSTREAM_BASE` |
| `prewarm.providers` | `[]` | `PREWARM_PROVIDERS` |
| `prewarm.platforms` | `[linux_amd64]` | `PREWARM_PLATFORMS` |

## Auth

| Value | Default | Notes |
|---|---|---|
| `auth.enabled` | `false` | |
| `auth.token` | `""` | Required when `auth.enabled` and no `existingSecret`. Ends up in Helm release history |
| `auth.existingSecret` | `""` | Secret holding key `AUTH_TOKEN`. Preferred |

## Storage

| Value | Default | Notes |
|---|---|---|
| `persistence.enabled` | `true` | `false` gives each pod an `emptyDir` — the HA mode |
| `persistence.storageClass` | `""` | Cluster default |
| `persistence.accessMode` | `ReadWriteOnce` | `ReadWriteMany` is what lets several replicas share one volume |
| `persistence.size` | `20Gi` | |
| `s3.enabled` | `false` | |
| `s3.bucket`, `s3.prefix`, `s3.endpoint`, `s3.region` | `""`, `tf-mirror`, `""`, `us-east-1` | `endpoint` switches on path-style addressing for MinIO/OVH |
| `s3.accessKey`, `s3.secretKey` | `""` | Both or neither. Empty means the AWS default credential chain (IRSA) |
| `s3.existingSecret` | `""` | Secret with `S3_ACCESS_KEY` / `S3_SECRET_KEY`. Preferred |

## Networking and observability

| Value | Default | Notes |
|---|---|---|
| `service.type` / `service.port` | `ClusterIP` / `80` | |
| `ingress.enabled` | `false` | Terminate TLS here — clients require an https mirror URL |
| `ingress.className`, `annotations`, `hosts`, `tls` | see values | Default host is `tf-mirror.internal` |
| `serviceMonitor.enabled` | `false` | Prometheus-operator scraping |
| `podDisruptionBudget.enabled` | `false` | Only meaningful with `replicaCount > 1` |
| `podDisruptionBudget.minAvailable` | `1` | |

## Security context

The pod runs as non-root uid 65532 with `fsGroup` set so the PVC is writable, a
`RuntimeDefault` seccomp profile, a read-only root filesystem, no privilege
escalation, and all capabilities dropped. Override via `podSecurityContext` and
`securityContext` only if your policies demand something different.

## Combinations the chart refuses

`replicaCount > 1` together with an enabled `ReadWriteOnce` PVC fails at render
time with an explanation. Two pods cannot share an RWO volume, so the chart
refuses to produce a Deployment that would wedge on the second replica: switch to
S3-backed mode (`persistence.enabled=false` + `s3.enabled=true`) or an RWX
storage class.
