# Security Policy

## Reporting a vulnerability

Please report security issues **privately** via GitHub Security Advisories
("Report a vulnerability" on the repository's Security tab), or by email to the
maintainer. Do not open a public issue for undisclosed vulnerabilities.

We aim to acknowledge reports within a few business days and to provide a fix or
mitigation timeline after triage.

## Supported versions

terrastrata is pre-1.0. Security fixes are applied to the latest released minor
version.

## Security model

terrastrata is designed to run on an **internal network**, in front of the public
Terraform registry.

- **No authentication by default.** Set `AUTH_TOKEN` to require a bearer token on
  the mirror endpoints. Note that Terraform's `network_mirror` client does *not*
  send authentication headers, so bearer auth is intended for gateways that
  inject the header or for non-Terraform consumers. Network policy / ingress
  controls remain the primary boundary.
- **Path traversal protection.** Every request coordinate (hostname, namespace,
  type, version, platform, filename) is strictly validated before it is used in a
  cache key or an upstream URL (`internal/mirror/paths.go`). The filesystem cache
  additionally contains all keys within its root directory.
- **Integrity verification.** Provider archives are verified against the
  registry-published SHA-256 checksum before they are cached or served.
- **Upstream TLS.** Upstream requests use TLS verification with bounded
  connection, TLS-handshake, and response-header timeouts.
- **Minimal runtime.** The container image is distroless (no shell, no package
  manager), runs as a non-root user with a read-only root filesystem and all
  Linux capabilities dropped.
- **Supply chain.** CI runs `govulncheck`, `golangci-lint` (including `gosec`),
  and a Trivy image scan on every change.

## Hardening recommendations

- Terminate TLS at your ingress/gateway; terrastrata serves plain HTTP.
- Restrict access with NetworkPolicy to your CI/CD agents.
- Pin the container image by digest in production.
