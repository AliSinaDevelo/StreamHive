# Changelog

All notable changes to StreamHive are documented here. This project follows [Semantic Versioning](https://semver.org/) for the **public Go API** (`p2p`, `storage`, and stable CLI flags). Until v1.0.0, minor releases may include API adjustments; see entries below.

## [0.2.0] — 2026-04-05

### Added

- **Semver** `Version` constant and this changelog.
- **`storage`**: `BlobStore` interface and in-memory implementation for content-keyed blobs.
- **`p2p`**: length-prefixed wire framing (`SHV1` magic), metrics, `context.Context` on `ListenAndAccept` / `Dial`, graceful accept-loop shutdown, peer removal on disconnect, optional max peers, dial / read idle deadlines, optional TLS, optional framed `FrameHandler`.
- **CLI**: `-version`, `-max-peers`, `-dial-timeout`, `-read-idle-timeout`, optional `-health` (live/ready/metrics), optional TLS flags.
- **Ops docs**: deployment (Docker/K8s sketch), governance (branch protection checklist), SBOM artifact in CI, pinned GitHub Actions by commit SHA.

### Changed

- **Breaking**: `Transport.ListenAndAccept` and `Dial` now take `context.Context` as the first argument.

## [0.1.0]

- Initial public foundation: TCP transport, tests, CI, documentation.
