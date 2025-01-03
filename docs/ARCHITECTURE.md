# Architecture

## Layers

1. **Transport (`p2p`)** — TCP listen/accept, outbound dial, peer lifecycle hooks. This is what exists today.
2. **Framing / protocol** (planned) — length-prefixed or streaming messages, handshake, backpressure.
3. **Storage** (planned) — content-addressed blobs or chunks, local index, replication metadata.

## Package map

| Path | Role |
|------|------|
| `p2p` | `Peer`, `Transport`, `TCPTransport`, `TCPPeer` |
| `.` | Thin CLI (`-listen`, `-dial`) for manual testing |

## Concurrency

`TCPTransport` uses an `RWMutex` so the accept loop can read the listener pointer without racing `Close`, which nils the listener and closes the underlying `net.Listener`. Peer bookkeeping uses the same lock as the peer map.

## Roadmap

- Binary wire format and request/response or stream abstraction
- Merkle or hash-linked chunk references
- Configurable replication and peer discovery (beyond static `-dial`)
