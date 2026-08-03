# Architecture

StreamHive is built in four layers — transport, framing, replication, and storage — each
a thin seam over the one below it. The diagram below is the fastest orientation; the
sections after it go deep.

```mermaid
flowchart TD
  cli["CLI / library user"] --> tr["Transport (p2p)<br/>TCPTransport · TLS · context"]
  tr --> fr["Framing (p2p)<br/>SHV1 length-prefixed frames"]
  fr --> rep["Replication (replication)<br/>blob.put / has / get / missing / ack"]
  rep --> st["Storage (storage)<br/>BlobStore"]
  st --> mem["MemoryStore"]
  st --> file["FileStore (durable)"]
  tr -. exposes .-> health["HTTP /livez /readyz /peers /metrics"]
```

## Layers

1. **Transport (`p2p`)** — `TCPTransport` with `context.Context` on `ListenAndAccept` / `Dial`, accept-loop shutdown coordinated with `Close`, optional TLS, optional shared-token peer auth with exchanged application identities, optional framed reads via `FrameHandler`, metrics, peer snapshots with auth-method and identity labels, and peer disconnect hooks.
2. **Framing (`p2p`)** — `SHV1` length-prefixed payloads (`ReadFrame` / `WriteFrame`) with a configurable maximum size (DoS bound). Application-level handshake string: `HandshakeVersionV1`.
3. **Replication (`replication`)** — typed JSON messages carried inside frames. `blob.put` writes one key/value blob to a receiving `BlobStore`; `blob.has`, `blob.get`, and `blob.missing` provide the inventory/request vocabulary for anti-entropy sync; `blob.ack` records accepted puts.
4. **Storage (`storage`)** — `BlobStore` interface with `BlobKeyLister` inventory support, `MemoryStore` for tests/demos, and `FileStore` for durable local blobs; SHA-256 helpers provide stable content-addressed keys.

## Package map

| Path | Role |
|------|------|
| `p2p` | `Peer`, `Transport`, `TCPTransport`, `TCPPeer`, wire framing |
| `replication` | Blob replication protocol, validation limits, apply helper |
| `storage` | `BlobStore`, `MemoryStore` |
| `internal/version` | Semver string for releases |
| `.` | CLI: `run`, health HTTP server, replication demo flags |

For the wire-level frame and message contract, see [PROTOCOL.md](PROTOCOL.md).

### Core interfaces

The seams are interfaces, so storage and peers are swappable without touching the layers
above. `TCPPeer` adds `WriteFrame` on top of the `Peer` contract.

```mermaid
classDiagram
  class Transport {
    <<interface>>
    +ListenAndAccept(ctx) error
    +Dial(ctx, address) error
    +Addr() net.Addr
    +Close() error
  }
  class Peer {
    <<interface>>
    +RemoteAddr() net.Addr
    +IsOutbound() bool
    +Close() error
  }
  class BlobStore {
    <<interface>>
    +Put(ctx, key, data) error
    +Get(ctx, key) bytes
    +Has(ctx, key) bool
    +Delete(ctx, key) error
  }
  class BlobKeyLister {
    <<interface>>
    +Keys(ctx) keys
  }
  class TCPPeer {
    +WriteFrame(payload, max) error
  }
  Transport <|.. TCPTransport
  Peer <|.. TCPPeer
  BlobStore <|.. MemoryStore
  BlobStore <|.. FileStore
  BlobKeyLister <|.. MemoryStore
  BlobKeyLister <|.. FileStore
  TCPTransport o-- TCPPeer : tracks
  TCPTransport ..> BlobStore : FrameHandler writes
```

## Concurrency and lifecycle

- Listener and peer map share a mutex; the accept loop exits when the listener is closed.
- `Close` stops new accepts, waits for the accept goroutine, then closes open peer connections. Peer goroutines remove themselves from the map on EOF / error via `unregisterPeer`.
- Optional `FrameHandler` runs per frame on each peer session until error, context cancellation, or disconnect. `TCPPeer.WriteFrame` serializes concurrent frame writers so ACK responses, retries, and inventory messages cannot interleave on one TCP stream.
- CLI replication installs a `FrameHandler` that decodes `blob.put` messages and writes to `MemoryStore` by default, or `FileStore` when `-store-dir` is set. Outbound `-put-key` / `-put-data` sends one manually keyed frame after `-dial` or `-peers` connects; `-put-content-key` derives the key from `SHA-256(-put-data)`. `-list-keys` inspects durable stores by printing known keys as hex.
- Receivers treat 32-byte keys as SHA-256 content addresses and verify payload integrity before storage. Exact duplicate key/data writes are skipped and counted separately; opaque keys with different data still replace existing values.
- `-peer-reconnect` manages only static `-peers` targets. It retries failed dials with exponential backoff and schedules another retry when an outbound configured peer disconnects.
- `-peer-auth-token` requires a shared-token auth frame before a peer is registered or allowed to exchange replication frames. The optional `-peer-id` is exchanged in that frame and retained as `auth_identity` on the remote peer. The default is unauthenticated for local demos. Connection logs and peer snapshots label the admission mode as `auth_method=none` or `auth_method=shared-token` and include the optional identity.
- Replication peers advertise local keys on connect. When `-sync-interval` is set, nodes also advertise local keys periodically to repair blobs added after peer startup. Receivers reply with `blob.missing`, owners send the requested blobs with `blob.put`, and receivers answer accepted puts with `blob.ack`. One-shot CLI puts track ACKs per peer/key and retry timed-out writes within a bounded budget; anti-entropy sends remain repairable through later inventory passes. Delivery logs classify accepted, ACK-timeout, write-error, and canceled outcomes, while aggregate counters avoid high-cardinality peer labels.

### Peer lifecycle

```mermaid
stateDiagram-v2
  [*] --> Dialing: outbound (-dial / -peers)
  [*] --> Accepting: inbound
  Dialing --> Auth: TCP + optional TLS handshake
  Auth --> Connected: optional peer auth accepted
  Auth --> Rejected: peer auth failed
  Dialing --> Backoff: dial failed (-peer-reconnect)
  Backoff --> Dialing: backoff elapsed
  Accepting --> Auth: accepted TCP socket
  Auth --> Connected: under max-peers
  Accepting --> Rejected: max-peers reached
  Connected --> Framed: FrameHandler installed
  Framed --> Framed: read + apply frame
  Connected --> Disconnected: EOF / error / Close
  Framed --> Disconnected: EOF / error
  Disconnected --> Backoff: was a configured -peers target
  Disconnected --> [*]
  Rejected --> [*]
```

## Failure modes (transport)

- **Dial** respects context cancellation and optional `DialTimeout`.
- **Max peers** rejects new inbound connections when the cap is reached (`PeersRejected` metric).
- **TLS** failures surface from `HandshakeContext` on outbound dials. **mTLS** is supported by configuring `tls.Config` yourself (`ClientAuth`, `ClientCAs` on `TLSServerConfig`; client certs on `TLSClientConfig`). TLS can authenticate certificates.
- **Peer auth** failures happen before peer registration when `PeerAuthToken` / `-peer-auth-token` is configured. Optional identities are bounded printable labels exchanged inside that authenticated handshake. This is shared-token admission plus identity labeling, not authorization, ACLs, or signed replication messages.
- **Replication decode/apply** rejects unknown message types, empty keys, oversized keys, and oversized payloads before writing to storage.

## Replication v0.3 scope

```mermaid
sequenceDiagram
  participant Sender
  participant SenderTransport as TCPTransport
  participant ReceiverTransport as TCPTransport
  participant Replication
  participant Store as BlobStore

  Sender->>SenderTransport: -dial receiver + -put-key/-put-data or -put-content-key
  SenderTransport->>ReceiverTransport: SHV1 frame carrying blob.put JSON
  ReceiverTransport->>Replication: FrameHandler(payload)
  Replication->>Replication: decode + validate limits
  Replication->>Store: Put(ctx, key, data)
  ReceiverTransport-->>SenderTransport: blob.ack for accepted key
  SenderTransport->>SenderTransport: match ACK or retry after bounded timeout
```

Implemented:

- Static peer replication over `-dial` and comma-separated `-peers`.
- Optional reconnect/backoff for `-peers`.
- Message types: `blob.put`, `blob.has`, `blob.get`, and `blob.missing`.
- Per-key `blob.ack` responses plus bounded ACK-driven retries for one-shot CLI puts.
- Delivery outcome logs and aggregate accepted/failure/write-error counters for one-shot CLI puts.
- Startup anti-entropy for connected `-replicate` peers.
- Receiver-side storage via `storage.MemoryStore` or durable `storage.FileStore` with `-store-dir`.
- JSON `/peers` snapshots for connected peer addresses, direction, connection timestamp, connection age, and `auth_method`.
- Optional `auth_identity` values in peer snapshots and structured connection logs.
- JSON `/metrics` counters for stored/sent blobs, ACKs, pending waiters, retry timeouts, bytes, duplicates, and replication errors.

Not implemented yet:

- Conflict resolution.
- Automated peer discovery beyond static dial targets.
- Per-peer authorization policy, ACLs, or signed replication messages beyond optional TLS/mTLS, shared-token peer auth, and exchanged identity labels.

## Storage choices

Use `MemoryStore` for tests, examples, and short-lived CLI demos. It copies values on `Put`/`Get`, is safe for concurrent access, and loses data when the process exits. CLI replication uses this by default.

Use `FileStore` when blobs must survive process restarts. Keys are hex-encoded into file names, writes use a temporary file followed by `rename`, and missing keys map to `storage.ErrNotFound`. CLI replication uses this when `-store-dir` is set. It is intentionally simple local storage, not a distributed database.

Use `storage.SHA256Key` or `storage.SHA256KeyHex` when the key should be derived from blob content instead of caller-chosen metadata.

## Roadmap

- Hash-linked chunk references on top of `BlobStore`
- Automated discovery beyond static peers
- Per-peer authorization policy, ACLs, or signed replication messages on top of the exchanged identity
