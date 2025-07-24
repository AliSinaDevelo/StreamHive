# StreamHive

[![CI](https://github.com/AliSinaDevelo/StreamHive/actions/workflows/ci.yml/badge.svg)](https://github.com/AliSinaDevelo/StreamHive/actions/workflows/ci.yml)

StreamHive is a **Go library and CLI** for experimenting with distributed, content-addressed storage. It ships a production-minded **TCP transport** (context-aware listen/dial, TLS hooks, framing, metrics, limits), a **length-prefixed wire format** (`SHV1`), an in-memory **blob store** API, and operational endpoints (`/livez`, `/readyz`, `/metrics`).

**Semver:** public API versions are tracked in [CHANGELOG.md](CHANGELOG.md) and [internal/version/version.go](internal/version/version.go) (currently **v0.2.0**, pre-1.0).

**Status:** networking + framing + local storage contract are implemented; replication and global discovery are not. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

- Go 1.22 or newer
- Optional: [golangci-lint](https://golangci-lint.run/) for `make lint`
- Optional: Docker for [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)

## Quickstart

```bash
go test ./...
go run . -version
make run
./bin/fs -listen :7070 -dial 127.0.0.1:8080
./bin/fs -listen 127.0.0.1:0 -health 127.0.0.1:8080   # HTTP live/ready/metrics
```

### Library packages

| Import | Purpose |
|--------|---------|
| `github.com/AliSinaDevelo/StreamHive/p2p` | `TCPTransport`, framing (`ReadFrame` / `WriteFrame`), metrics |
| `github.com/AliSinaDevelo/StreamHive/storage` | `BlobStore`, `MemoryStore` |

Wire handshake string constant: `p2p.HandshakeVersionV1` (carry inside application frames).

## CLI flags (stable surface)

| Flag | Meaning |
|------|---------|
| `-listen` | TCP listen address |
| `-dial` | Optional peer to dial after listen |
| `-health` | HTTP `host:port` for `/livez`, `/readyz`, `/metrics` |
| `-max-peers` | Cap simultaneous peers (0 = unlimited) |
| `-dial-timeout` | Outbound dial timeout |
| `-read-idle-timeout` | Peer read deadline refresh |
| `-tls-cert` / `-tls-key` | Server TLS |
| `-tls-ca` / `-tls-server-name` / `-tls-insecure-skip-verify` | Client TLS |

See the [Makefile](Makefile) for `test-race`, `vet`, `cover`, and `lint`.

## Architecture (summary)

```mermaid
flowchart TB
  subgraph app [Process]
    CLI[CLI / run]
    T[TCPTransport]
    F[SHV1 frames]
    S[BlobStore]
    CLI --> T
    T --> F
    CLI -. planned .-> S
  end
  T <-->|TCP/TLS| remote[Remote peers]
```

## Operations & supply chain

- CI pins third-party GitHub Actions to immutable commit SHAs and uploads **coverage** plus a **CycloneDX SBOM** (`sbom` job).
- [docs/WORKFLOWS.md](docs/WORKFLOWS.md) — local and CI expectations.
- [docs/GOVERNANCE.md](docs/GOVERNANCE.md) — branch protection and release hygiene.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) — Docker and Kubernetes sketch.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security: [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
