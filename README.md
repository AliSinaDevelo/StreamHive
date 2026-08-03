# StreamHive

[![CI](https://github.com/AliSinaDevelo/StreamHive/actions/workflows/ci.yml/badge.svg)](https://github.com/AliSinaDevelo/StreamHive/actions/workflows/ci.yml)

StreamHive is a **Go library and CLI** for experimenting with distributed, content-addressed storage. It ships a production-minded **TCP transport** (context-aware listen/dial, TLS hooks, optional shared-token peer auth, framing, metrics, limits), a **length-prefixed wire format** (`SHV1`), a typed **blob replication protocol**, memory and file-backed **blob stores**, and operational endpoints (`/livez`, `/readyz`, `/peers`, `/metrics`, `/metrics/prometheus`).

**Semver:** public API versions are tracked in [CHANGELOG.md](CHANGELOG.md) and [internal/version/version.go](internal/version/version.go) (currently **v0.7.0**, pre-1.0).

**Status:** networking, framing, local storage, content-addressed blob keys, static-peer replication, bounded ACK-driven retries for one-shot puts, startup and periodic anti-entropy sync, durable stores, self-repair demos, and Prometheus metrics are implemented. `storage.FileStore` provides durable local blobs for library users and CLI receivers via `-store-dir`. Conflict resolution and global discovery are not implemented. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

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
./bin/fs -listen 127.0.0.1:0 -health 127.0.0.1:8080   # HTTP live/ready/peers/metrics
```

### Two-node replication demo

Terminal 1: start a receiver with framed replication and metrics.

```bash
go run . -listen 127.0.0.1:7070 -replicate -health 127.0.0.1:8080
```

Terminal 2: dial the receiver and send one blob.

```bash
go run . -listen 127.0.0.1:0 -dial 127.0.0.1:7070 -put-content-key -put-data "hello streamhive" -exit-after-put
```

Inspect counters:

```bash
curl -s http://127.0.0.1:8080/metrics
```

Inspect connected peers:

```bash
curl -s http://127.0.0.1:8080/peers
```

Look for `replication_blobs_stored`, `replication_bytes_stored`, `replication_blob_acks_received`, `replication_blob_acks_matched`, `replication_blob_ack_timeouts`, `replication_blob_retries`, `replication_blob_puts_accepted`, `replication_blob_put_failures`, and `replication_blob_write_errors`, plus duplicate/skipped, auth, and transport frame counters. The sender derives the blob key from `SHA-256(put-data)` when `-put-content-key` is set; receivers verify SHA-256-shaped keys before storing. One-shot sends wait for a matching `blob.ack` and retry the idempotent `blob.put` within the configured budget. Structured delivery logs include the remote peer, key, outcome, and attempt count. Use `/metrics` for JSON counters, `/metrics/prometheus` for Prometheus text format, and `/peers` for sorted peer metadata including remote address, local address, direction, connection timestamp, connection age, and `auth_method` (`none` or `shared-token`).

Or run the whole flow:

```bash
make demo-replication
```

For a 3-node Docker Compose demo with durable stores and node restart rehydration:

```bash
make demo-compose
```

For the authenticated Compose variant, set one shared token. The demo proves matching
tokens replicate and a wrong token is rejected before the blob reaches durable storage:

```bash
STREAMHIVE_PEER_TOKEN="replace-with-a-local-demo-token" make demo-auth
```

To inspect a running Compose cluster:

```bash
make demo-status
```

For a 3-node corruption repair demo that deletes node3's durable blob and waits for periodic anti-entropy to restore it:

```bash
make demo-repair
```

For a reconnect/failure demo that stops node2, deletes its durable blob while down, restarts it, and waits for peer reconnect plus repair:

```bash
make demo-failure
```

For a longer-lived node with static peers, use `-peers` and reconnect backoff:

```bash
go run . -listen 127.0.0.1:7071 -peers 127.0.0.1:7070,127.0.0.1:7072 -peer-reconnect
```

`-peer-reconnect` retries only `-peers` targets. `-dial` stays a one-shot connection attempt for scripts and tests.

When both sides run with `-replicate`, peers advertise local keys on connect and send missing blobs to each other. This startup anti-entropy path works with memory storage and durable `-store-dir` receivers.

For long-running nodes, add periodic inventory sync:

```bash
go run . -listen 127.0.0.1:7071 -replicate -store-dir ./streamhive-data -peers 127.0.0.1:7070 -peer-reconnect -sync-interval 30s
```

For a private local cluster, require a shared peer auth token on every node:

```bash
go run . -listen 127.0.0.1:7070 -replicate -peer-auth-token "$STREAMHIVE_PEER_TOKEN"
go run . -listen 127.0.0.1:7071 -replicate -dial 127.0.0.1:7070 -peer-auth-token "$STREAMHIVE_PEER_TOKEN"
```

Peers that cannot complete the auth handshake are rejected before replication frames reach the application handler. Use TLS or mTLS as well when the token crosses a network you do not fully control.

To persist replicated blobs on the receiver, add `-store-dir`:

```bash
go run . -listen 127.0.0.1:7070 -replicate -store-dir ./streamhive-data
```

### Library packages

| Import | Purpose |
|--------|---------|
| `github.com/AliSinaDevelo/StreamHive/p2p` | `TCPTransport`, framing (`ReadFrame` / `WriteFrame`), optional peer auth, peer snapshots, metrics |
| `github.com/AliSinaDevelo/StreamHive/replication` | Blob replication messages (`blob.put`, `blob.has`, `blob.get`, `blob.missing`, `blob.ack`) and store apply helper |
| `github.com/AliSinaDevelo/StreamHive/storage` | `BlobStore`, `BlobKeyLister`, `MemoryStore`, `FileStore`, SHA-256 content key helpers |

Wire handshake string constant: `p2p.HandshakeVersionV1` (carry inside application frames).

## CLI flags (stable surface)

| Flag | Meaning |
|------|---------|
| `-listen` | TCP listen address |
| `-dial` | Optional peer to dial after listen |
| `-peers` | Optional comma-separated peers to dial after listen |
| `-peer-reconnect` | Retry `-peers` with exponential backoff |
| `-peer-reconnect-min` / `-peer-reconnect-max` | Reconnect backoff bounds |
| `-sync-interval` | Periodically advertise local blob keys to connected peers (0 = startup only) |
| `-health` | HTTP `host:port` for `/livez`, `/readyz`, `/peers`, `/metrics` |
| `-max-peers` | Cap simultaneous peers (0 = unlimited) |
| `-peer-auth-token` / `-peer-auth-timeout` | Optional shared-token peer auth before peer registration |
| `-dial-timeout` | Outbound dial timeout |
| `-read-idle-timeout` | Peer read deadline refresh |
| `-tls-cert` / `-tls-key` | Server TLS |
| `-tls-ca` / `-tls-server-name` / `-tls-insecure-skip-verify` | Client TLS |
| `-replicate` | Enable blob replication from framed peers |
| `-store-dir` | Persist replicated blobs with `storage.FileStore` |
| `-list-keys` | Print durable `-store-dir` keys as hex and exit |
| `-put-key` / `-put-data` | Send one manually keyed blob to outbound peers |
| `-put-content-key` | Derive the outbound blob key from `SHA-256(-put-data)` |
| `-put-ack-timeout` | Time to wait for each one-shot blob acknowledgment |
| `-put-retries` | Additional one-shot blob sends after an ACK timeout (0-10) |
| `-put-retry-delay` | Initial delay before retrying an unacknowledged blob (backoff capped at 500ms) |
| `-exit-after-put` | Wait for matching acknowledgments from all outbound peers, then exit |
| `-max-blob-bytes` | Cap replicated blob payload size |

See the [Makefile](Makefile) for `test-race`, `vet`, `cover`, `lint`, and demos.

## Architecture (summary)

```mermaid
flowchart TB
  subgraph app [Process]
    CLI[CLI / run]
    T[TCPTransport]
    F[SHV1 frames]
    S[BlobStore]
    R[replication blob.put / blob.has / blob.ack]
    CLI --> T
    T --> F
    F --> R
    R --> S
  end
  T <-->|TCP/TLS| remote[Remote peers]
```

## Operations & supply chain

- CI pins third-party GitHub Actions to immutable commit SHAs and uploads **coverage** plus a **CycloneDX SBOM** (`sbom` job).
- [docs/WORKFLOWS.md](docs/WORKFLOWS.md) — local and CI expectations.
- [docs/GOVERNANCE.md](docs/GOVERNANCE.md) — branch protection and release hygiene.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) — Docker and Kubernetes sketch.
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — frame format, replication messages, limits, and repair behavior.
- [docs/RELEASE.md](docs/RELEASE.md) — release checklist.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security: [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
