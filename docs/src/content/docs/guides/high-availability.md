---
title: High availability
description: Running several replicas, and the volume constraint that shapes how.
---

The default deployment is a single replica with a `ReadWriteOnce` PVC. That is a
deliberate default rather than a limitation of the software: an RWO volume cannot
be shared, so a second pod would sit unschedulable or fight the first for the
disk.

## S3-backed mode

For HA, each pod keeps its own **ephemeral** local cache and shares durability
through S3:

```bash
helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata \
  --namespace tf-mirror --create-namespace \
  --set replicaCount=3 \
  --set persistence.enabled=false \
  --set s3.enabled=true \
  --set s3.bucket=tf-mirror \
  --set s3.endpoint=https://s3.de.io.cloud.ovh.net \
  --set s3.region=de \
  --set s3.existingSecret=tf-mirror-s3 \
  --set podDisruptionBudget.enabled=true
```

With `replicaCount > 1` the chart:

- switches the Deployment from `Recreate` to a rolling update,
- gives each pod an `emptyDir` local cache instead of the shared PVC,
- injects a **soft** pod anti-affinity so replicas spread across nodes (override
  with `affinity` or `topologySpreadConstraints`),
- renders a `PodDisruptionBudget` when you enable it.

## The guard rail

Asking for `replicaCount > 1` while keeping an enabled RWO PVC fails at render
time with an explanation. The chart refuses to generate a Deployment that would
wedge on its second replica. Your options are S3-backed mode, or a
`ReadWriteMany` storage class via `persistence.accessMode`.

## What HA costs you

Request coalescing is per-process, so a cold object is fetched **once per
replica** rather than once overall — three replicas can mean three upstream
fetches of the same new provider version, spread over whenever each first sees a
request for it. The durable layer bounds this: whichever replica fetches first
writes to S3, and the others find it there instead of going upstream on their
next miss.

If you would rather have one warm cache than several, a single replica on an RWX
volume is a legitimate configuration too — it just trades availability during a
node drain for cache locality.

## Pre-warming with several replicas

Every replica runs its own pre-warm at startup, so `PREWARM_PROVIDERS` costs one
pass per pod on a cold bucket and roughly one S3 read per pod afterwards. Keep the
list to the providers your pipelines actually block on rather than everything you
use.
