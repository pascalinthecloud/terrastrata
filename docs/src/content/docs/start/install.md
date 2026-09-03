---
title: Install
description: Deploy terrastrata to Kubernetes with Helm or raw manifests.
---

terrastrata is a single static binary in a distroless image. The chart and the
image are both published to GHCR and cosign-signed.

## With Helm

```bash
helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata \
  --version 0.5.2 \
  --namespace tf-mirror --create-namespace
```

That gets you a local-disk cache on a 20Gi `ReadWriteOnce` PVC, one replica, and
`registry.terraform.io` as the upstream. Every knob is listed under
[Helm values](/terrastrata/reference/helm-values/).

### With a durable S3 layer

Add object storage and the cache survives pod restarts and node moves:

```bash
helm install tf-mirror oci://ghcr.io/pascalinthecloud/charts/terrastrata \
  --namespace tf-mirror --create-namespace \
  --set s3.enabled=true \
  --set s3.bucket=tf-mirror \
  --set s3.endpoint=https://s3.de.io.cloud.ovh.net \
  --set s3.region=de \
  --set s3.existingSecret=tf-mirror-s3
```

Prefer `s3.existingSecret` (keys `S3_ACCESS_KEY` / `S3_SECRET_KEY`) over inline
values: inline credentials end up in Helm release history and your shell history.
On AWS you can skip credentials entirely — leave `s3.accessKey` empty and attach
an IRSA role through `serviceAccount.annotations`, and the AWS default credential
chain takes over.

## With raw manifests

```bash
# Fill in S3 credentials first if you want the durable layer
kubectl apply -f deploy/k8s/manifests.yaml
```

The manifests create a namespace, PVC, Deployment, and Service, with an example
Ingress commented out.

## Verify what you are running

Both artifacts are signed with cosign (keyless, via Sigstore):

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/pascalinthecloud/terrastrata/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pascalinthecloud/terrastrata:0.5.2
```

Releases also ship a bill of materials for exactly what is inside the image — see
[supply chain](/terrastrata/guides/supply-chain/). Pin by digest in production.

## Sizing the volume

`hashicorp/azurerm` alone can reach 30–50 GB if every version ends up cached, so
size the PVC for your provider set and set `CACHE_MAX_BYTES` a few GB below it.
The chart defaults to a 20Gi volume with an 18GB budget, so eviction kicks in
before the volume fills rather than after.

## Next

Point your clients at it: [client setup](/terrastrata/start/clients/).
