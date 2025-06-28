# Governance

## Branch protection (recommended)

For the canonical GitHub repository, enable:

- Require pull request reviews before merging (at least one approval for shared repos).
- Require status checks to pass (`CI` workflow) before merge.
- Require branches to be up to date before merging.
- Include administrators where policy allows.
- Restrict who can push to `main` (admins / release role only).

## Releases

- Tag releases as `vMAJOR.MINOR.PATCH` aligned with [CHANGELOG.md](../CHANGELOG.md) and [internal/version/version.go](../internal/version/version.go).
- Prefer signed tags (`git tag -s`) for public releases.
- Attach `sbom.cdx.json` from CI artifacts to release notes when publishing binaries.

## Signed artifacts

This repository does not yet automate Sigstore/cosign signing. For production images, sign with your registry’s policy (e.g. Cosign, Notary).
