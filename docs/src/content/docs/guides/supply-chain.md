---
title: Supply chain
description: Verifying what you run, the bills of material shipped with each release, and how dependencies are scanned.
---

## Verify the image

Images are multi-arch (`linux/amd64`, `linux/arm64`) on a distroless static base,
built with provenance, and signed with cosign (keyless, via Sigstore):

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/pascalinthecloud/terrastrata/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pascalinthecloud/terrastrata:0.5.2
```

The Helm chart published to `oci://ghcr.io/pascalinthecloud/charts/terrastrata` is
signed the same way. Pin by digest in production.

## Bills of material

Every GitHub Release attaches:

| File | Format | Describes |
|---|---|---|
| `terrastrata-linux-amd64.spdx.json` | SPDX 2.3 | everything inside the pushed image for that platform — Go modules plus the distroless base |
| `terrastrata-linux-arm64.spdx.json` | SPDX 2.3 | the same, for arm64 |
| `terrastrata.cdx.json` | CycloneDX 1.6 | the binary's own dependency graph, with a detected licence for every component |

The SPDX documents are the ones buildkit produced at build time and keeps in the
registry as an attestation — the release assets are a copy you can download
without a registry client. To read them straight from the registry instead:

```bash
docker buildx imagetools inspect ghcr.io/pascalinthecloud/terrastrata:0.5.2 \
  --format '{{ json (index .SBOM "linux/amd64").SPDX }}'
```

Licences in the CycloneDX document are *detected* rather than declared, so they
live under `components[].evidence.licenses` as CycloneDX intends — not
`components[].licenses`, which stays empty. Reading the wrong field makes the BOM
look licence-free:

```bash
jq -r '.components[] | "\(.name) \(.evidence.licenses[0].license.id)"' terrastrata.cdx.json
```

CI also uploads a CycloneDX SBOM as a build artifact on every run, so a dependency
change is visible per commit rather than only per release.

## How dependencies are scanned

Four checks run on every pull request, each answering a different question:

| Check | Scope | Question |
|---|---|---|
| `govulncheck` | Go source | is a known vulnerability **reachable** from our code? |
| `osv-scanner` | `go.mod` | is any dependency **version** affected, reachable or not? |
| Dependency review | the PR's diff | does this PR *introduce* a high-severity advisory or a copyleft licence? |
| Trivy | the built image | anything vulnerable in the image, including the base layer? |

`govulncheck` is call-graph aware and therefore quiet enough to gate on;
`osv-scanner` deliberately is not, because a vulnerable module sitting unused in
the graph is still the first thing an auditor asks about. Dependabot opens the
update PRs weekly for Go modules, GitHub Actions, and the base image, and every
action is pinned to a full commit SHA rather than a mutable tag.

## What the cache itself verifies

- **Provider zips** are checked against the registry-published SHA-256 before
  being cached or served; a mismatch or a missing checksum is refused.
- **Requested filenames** must match what the registry publishes, so a client
  cannot cause an archive to be cached under a name of its choosing.
- **Module archives** cannot be checksum-verified — the protocol publishes none —
  but their entry names and link targets are validated so a malicious module
  cannot escape its extraction directory. See
  [module registry](/terrastrata/guides/module-registry/).
- **Foreign hostnames** are refused rather than proxied, so no upstream's content
  can be cached under another's keys.

## Reporting something

Security issues go through the
[security policy](https://github.com/pascalinthecloud/terrastrata/blob/main/SECURITY.md)
rather than a public issue.
