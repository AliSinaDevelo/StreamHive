# Protocol Reference

StreamHive carries replication messages inside `SHV1` frames over a connected TCP peer.
This document describes the current wire contract for operators and library users.

## Frame Format

Each frame has an 8-byte header followed by an opaque payload:

| Field | Size | Encoding | Meaning |
|-------|------|----------|---------|
| Magic | 4 bytes | ASCII | Must be `SHV1` |
| Length | 4 bytes | big-endian uint32 | Payload length in bytes |
| Payload | `Length` bytes | protocol-specific | Replication messages are JSON |

`p2p.ReadFrame` and `p2p.WriteFrame` default to `p2p.DefaultMaxFrameBytes`, currently
`4 << 20` bytes. A frame with a bad magic value fails with `p2p.ErrBadMagic`. A frame
whose declared payload length exceeds the configured maximum fails with
`p2p.ErrFrameTooLarge`.

## Peer Auth Handshake

By default, StreamHive keeps the application protocol open for local demos and tests. When
`p2p.TCPTransport.PeerAuthToken` or the CLI `-peer-auth-token` flag is set, a peer must
complete a shared-token auth handshake before it is registered or allowed to exchange
replication frames.

The dialer sends the first `SHV1` frame:

```json
{
  "type": "peer.auth",
  "version": "streamhive/1",
  "token": "shared-token",
  "identity": "node-b"
}
```

The listener validates the token and replies:

```json
{
  "type": "peer.auth.ok",
  "version": "streamhive/1",
  "identity": "node-a"
}
```

If validation fails, the listener may reply with `peer.auth.reject` and closes the
connection. Dialers treat rejected or malformed auth replies as dial failures. Auth
successes and failures are exposed as `peer_auth_success` and `peer_auth_failures`.

The `identity` fields are optional for compatibility with token-only peers. They provide
an explicit application identity label for snapshots and logs, but this slice does not
authorize identities by default or provide signed identity claims. To authorize inbound
identities, configure `TCPTransport.PeerAuthAllowedIdentities` or the CLI
`-peer-allow-ids` flag. Entries use exact matching; a missing or unlisted identity is
rejected before peer registration. An empty allowlist preserves token-only and
identity-label-only deployments. Use TLS or mTLS whenever the token or identity crosses a
network boundary where passive capture or active interception is possible.

## Replication Payloads

Replication payloads are JSON values decoded into `replication.Message`:

```json
{
  "type": "blob.put",
  "key": "base64-encoded-by-json",
  "data": "base64-encoded-by-json"
}
```

The Go `encoding/json` package encodes `[]byte` fields as base64 strings. This applies
to `key`, `keys`, and `data` fields on the wire.

## Message Types

| Type | Fields | Meaning |
|------|--------|---------|
| `blob.put` | `key`, `data` | Store or replace one blob under `key`. |
| `blob.has` | `keys` | Advertise keys available on the sender. |
| `blob.missing` | `keys` | Ask the peer to send keys missing locally. |
| `blob.get` | `key` | Ask the peer to send one key. |
| `blob.ack` | `key` | Acknowledge that one `blob.put` key was accepted. |

The CLI replication handler uses `blob.has` and `blob.missing` for anti-entropy:

1. A peer advertises local keys on connect.
2. When `-sync-interval` is set, a peer advertises local keys periodically.
3. A receiver computes which advertised keys it lacks and sends `blob.missing`.
4. The owner answers with `blob.put` for keys it can still read.
5. The receiver answers accepted `blob.put` messages with `blob.ack`.

## Limits

Default replication limits are:

| Limit | Value | Error |
|-------|-------|-------|
| Max key size | 512 bytes | `replication.ErrKeyTooLarge` |
| Max keys per inventory message | 4096 | `replication.ErrTooManyKeys` |
| Max blob payload | `4 << 20` bytes | `replication.ErrDataTooLarge` |

Empty keys fail with `replication.ErrKeyEmpty`. Empty `keys` lists fail with
`replication.ErrKeysEmpty`. Unknown message types fail with
`replication.ErrUnknownMessageType`.

## Content Addressing

`-put-content-key` stores a blob under `SHA-256(data)`. The CLI logs SHA-256 keys as
hex for readability. On receive, any 32-byte key is treated as a SHA-256 content key
and verified against the payload before storage. A mismatch fails with
`storage.ErrSHA256Mismatch` and the blob is not stored.

Opaque caller-chosen keys are still allowed. If an opaque key receives different data,
the existing value is replaced. If an exact key/data pair is received again, the write is
skipped and duplicate counters are incremented.

## Failure Behavior

Peer auth failures, frame decode errors, message validation errors, and peer write errors
stop the current peer loop. Auth failures happen before peer registration; later frame
failures unregister the peer and update metrics.

When answering `blob.missing` or `blob.get`, StreamHive treats each requested key as an
independent send unit until bytes are written to the peer. If one key is unreadable or
cannot be encoded under the configured limits, that key is skipped, `replication_send_errors`
and `replication_blobs_skipped` are incremented, and later requested keys are still sent.
If writing a frame to the TCP stream fails, the peer loop stops instead of retrying the
same frame on a potentially partial stream.

StreamHive sends `blob.ack` after a `blob.put` is stored or recognized as an exact
duplicate. For one-shot CLI puts, the sender registers the `(peer, key)` pair before
writing the frame and waits up to `-put-ack-timeout` for the matching ACK. A timeout
removes the pending entry, increments `replication_blob_ack_timeouts`, and retries the
same idempotent `blob.put` up to `-put-retries` additional times. `-put-retry-delay`
starts the bounded exponential delay between attempts; the delay is capped at 500ms.
`-exit-after-put` returns only after every outbound target acknowledges the key or its
retry budget is exhausted. `replication_blob_acks_received` counts every received ACK,
while `replication_blob_acks_matched` counts ACKs that completed a pending one-shot
send. `replication_blob_acks_pending` exposes current waiters. Inventory and requested
blob sends remain one-way write units; later anti-entropy or reconnect passes repair a
send that fails outside the one-shot ACK path.

One-shot delivery logs use `outcome=accepted`, `outcome=ack-timeout`,
`outcome=write-error`, or `outcome=canceled` and include the remote peer, key, and
attempt count. Aggregate counters expose the same operational split without putting
peer addresses into metric labels: `replication_blob_puts_accepted`,
`replication_blob_put_failures`, and `replication_blob_write_errors`. A write failure
closes the peer because the stream may contain a partial frame; it is never retried on
that connection.

Repair is driven by startup inventory, periodic inventory with `-sync-interval`, and
reconnect behavior for static `-peers` when `-peer-reconnect` is enabled. If a blob send
fails mid-sync, a later inventory pass or reconnect can request the missing key again.
When a store contains more keys than one inventory message permits, the owner emits
multiple bounded `blob.has` frames at the configured `MaxKeys` limit instead of failing
the entire advertisement. Each frame must also fit the transport `MaxFrameBytes` limit;
the sender adaptively splits a batch further when necessary, while a single key that
cannot fit fails the inventory pass with a frame-size error.
The receiver checks each advertised key with its `BlobStore.Has` operation, so the work
and temporary key set are bounded by the incoming inventory frame rather than the full
local store size.
Aggregate anti-entropy counters make that control loop visible without peer labels:
`replication_inventory_advertisements` counts successful `blob.has` frames,
`replication_missing_keys_requested` counts keys in `blob.missing` messages, and
`replication_repair_blobs_sent` counts successful repair `blob.put` frames. Repair
delivery logs use `delivery=anti-entropy`; one-shot CLI deliveries use
`delivery=one-shot`. Requested-blob delivery checks cancellation between store reads,
encoding, and frame writes, so shutdown stops a long repair pass at the next safe
boundary. Ordinary missing or unreadable blobs remain skip-and-continue cases and can
be requested again by a later inventory pass.

## Observability

Use `/metrics` for JSON counters, `/metrics/prometheus` for Prometheus text format, and
`/peers` for connected peer metadata. `/metrics` includes peer auth, transport, and
replication counters, including `peer_auth_identity_rejections` for inbound identity
allowlist failures and the aggregate anti-entropy counters described above. `/peers`
includes remote address, local address, direction,
connection timestamp, connection age in milliseconds, `auth_method` (`none` or
`shared-token`), and optional `auth_identity`.
