# Architecture

## Layers

1. **Transport (`p2p`)** — `TCPTransport` with `context.Context` on `ListenAndAccept` / `Dial`, accept-loop shutdown coordinated with `Close`, optional TLS, optional framed reads via `FrameHandler`, metrics, and peer disconnect hooks.
2. **Framing (`p2p`)** — `SHV1` length-prefixed payloads (`ReadFrame` / `WriteFrame`) with a configurable maximum size (DoS bound). Application-level handshake string: `HandshakeVersionV1`.
3. **Storage (`storage`)** — `BlobStore` interface with `MemoryStore` for tests and single-node demos; content addressing can layer hashes as opaque keys.

## Package map

| Path | Role |
|------|------|
| `p2p` | `Peer`, `Transport`, `TCPTransport`, `TCPPeer`, wire framing |
| `storage` | `BlobStore`, `MemoryStore` |
| `internal/version` | Semver string for releases |
| `.` | CLI: `run`, health HTTP server, flags |

## Concurrency and lifecycle

- Listener and peer map share a mutex; the accept loop exits when the listener is closed.
- `Close` stops new accepts, waits for the accept goroutine, then closes open peer connections. Peer goroutines remove themselves from the map on EOF / error via `unregisterPeer`.
- Optional `FrameHandler` runs per frame on each peer session until error, context cancellation, or disconnect.

## Failure modes (transport)

- **Dial** respects context cancellation and optional `DialTimeout`.
- **Max peers** rejects new inbound connections when the cap is reached (`PeersRejected` metric).
- **TLS** failures surface from `HandshakeContext` on outbound dials. **mTLS** is supported by configuring `tls.Config` yourself (`ClientAuth`, `ClientCAs` on `TLSServerConfig`; client certs on `TLSClientConfig`). There is no application-level identity beyond TLS yet.

## Roadmap

- Merkle or hash-linked chunk references on top of `BlobStore`
- Replication and discovery beyond static `-dial`
- Authenticated application protocol on top of `FrameHandler`
