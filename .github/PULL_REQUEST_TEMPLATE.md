<!-- Thanks for contributing! Keep the description focused on the what and why. -->

## What & why

<!-- What does this change do, and what problem does it solve? Link any related issue (Closes #N). -->

## How

<!-- Notable implementation details, trade-offs, or design decisions a reviewer should know. -->

## Checklist

- [ ] `make test lint vuln` pass locally
- [ ] Tests added/updated for behavioral changes
- [ ] Docs updated (README / CLAUDE.md / Helm values) if behavior or config changed
- [ ] New request coordinates (if any) go through `internal/mirror/paths.go` validation
- [ ] No secrets, credentials, or customer data in the diff or logs
