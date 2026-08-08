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

The authenticated envelope optionally carries a bounded capability list:

```json
{
  "type": "peer.auth",
  "version": "streamhive/1",
  "token": "shared-token",
  "capabilities": ["lifecycle.v1"]
}
```

`TCPTransport.PeerAuthCapabilities` advertises locally supported capabilities. The current
implementation supports `lifecycle.v1`; an incoming unknown capability is ignored for forward
compatibility, while duplicate, invalid, or oversized declarations fail authentication. A local
unknown capability is rejected during transport configuration. The list is exchanged only with
shared-token auth, is bounded independently from raw replication frames, and is exposed in
`TCPPeer.AuthCapabilities`, `PeerSnapshot.Capabilities`, and the health `/peers` response.

The capability status is explicit: `ready` means `lifecycle.v1` was negotiated,
`optional-raw-only` means a lifecycle-optional namespace may continue raw blob replication, and
`required-unavailable` means a lifecycle-required namespace must refuse lifecycle exchange. A
peer without the capability never receives a lifecycle record frame.

The `identity` fields are optional for compatibility with token-only peers. They provide
an explicit application identity label for snapshots and logs, but this slice does not
authorize identities by default or provide signed identity claims. To authorize inbound
identities, configure `TCPTransport.PeerAuthAllowedIdentities` or the CLI
`-peer-allow-ids` flag. Entries use exact matching; a missing or unlisted identity is
rejected before peer registration. An empty allowlist preserves token-only and
identity-label-only deployments. Use TLS or mTLS whenever the token or identity crosses a
network boundary where passive capture or active interception is possible.

TLS is a transport boundary around these frames, not a replacement for the application
handshake. The CLI listener uses `-tls-cert` and `-tls-key`; an outbound CLI peer uses
`-tls-ca` and `-tls-server-name` for certificate-chain and hostname verification. TLS must
complete before the shared-token handshake, identity allowlist, and peer registration can
succeed. See [TLS_AUTH.md](TLS_AUTH.md) for the executable ordering and library mTLS boundary.

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

Lifecycle records use a separate envelope after `lifecycle.v1` negotiation:

```json
{
  "type": "lifecycle.record",
  "record": {
    "namespace": "base64-encoded-by-json",
    "logical_key": "base64-encoded-by-json",
    "state": "present",
    "blob_key": "base64-encoded-32-byte-sha256",
    "version": {"epoch": 4, "sequence": 9},
    "authority_id": "node-a"
  }
}
```

`internal/lifecycle.DecodeRecord` rejects unknown message types, unknown envelope fields,
trailing JSON, malformed records, and oversized payloads before any record application.
`DecodeRecordForPeer` refuses the payload before decoding when `lifecycle.v1` is absent.
`internal/lifecycle.Applier` then verifies present-record bytes, writes supplied bytes to the raw
store before appending the durable journal and publishing state, and applies deletes as
tombstones without calling raw blob deletion. The internal `RepairBatch`, `RepairSnapshot`,
`WatermarkBook`, and `RepairCoordinator` APIs provide bounded journal planning and durable
per-peer acknowledgements. `RepairFrame` adds bounded batch, snapshot, and watermark-ack payload
codecs; `DecodeRepairFrameForPeer` refuses them before decoding without negotiated
`lifecycle.v1`. This is a caller-owned frame boundary: the current CLI does not schedule these
frames automatically. Compaction, CLI configuration, and physical blob deletion remain separate
work.

Lifecycle repair payloads use these JSON message types when a caller sends them through an
authenticated, capability-ready peer:

| Type | Fields | Meaning |
|------|--------|---------|
| `lifecycle.repair.batch` | `from`, `to`, `more`, `records` | Ordered journal prefix after a peer watermark. |
| `lifecycle.repair.snapshot` | `watermark`, `records` | Complete bounded logical checkpoint fallback. |
| `lifecycle.repair.ack` | `watermark` | Receiver acknowledgement after durable apply. |

The codec bounds entry count, logical-key bytes, metadata bytes, and encoded frame bytes
independently from raw blob limits. `internal/lifecycle.RepairSession` is the caller-owned
delivery loop: `SendNext` writes one bounded batch or snapshot, and `Handle` applies a received
frame through the verified applier before persisting its acknowledgement. Missing or corrupt
present-record blobs therefore produce no logical publication or acknowledgement. Batch delivery
still goes through the internal duplicate, gap, and reorder classifier; decoding alone never
mutates the journal, logical store, or raw blob store. A session reloads its peer watermark on
construction, so reconnect and process restart resume from the last durable acknowledgement.
The current CLI does not construct sessions or schedule lifecycle frames automatically.

## Message Types

| Type | Fields | Meaning |
|------|--------|---------|
| `blob.put` | `key`, `data` | Store or replace one blob under `key`. |
| `blob.has` | `keys` | Advertise keys available on the sender. |
| `blob.missing` | `keys` | Ask the peer to send keys missing locally. |
| `blob.get` | `key` | Ask the peer to send one key. |
| `blob.ack` | `key` | Acknowledge that one `blob.put` key was accepted. |
| `lifecycle.record` | `record` | One logical lifecycle mutation; requires `lifecycle.v1` and is not applied by the current transport slice. |

The CLI replication handler uses `blob.has` and `blob.missing` for anti-entropy:

1. A peer advertises local keys on connect.
2. When `-sync-interval` is set, a peer advertises local keys periodically.
3. A receiver computes which advertised keys it lacks and sends `blob.missing`.
4. The owner answers with `blob.put` for keys it can still read.
5. If the repair byte budget defers keys, the owner schedules one bounded continuation.
6. The receiver answers accepted `blob.put` messages with `blob.ack`.

Inventory enumeration is an internal storage API and does not change the wire format.
Stores implementing `storage.BlobKeyPager` return at most the requested page size in
bytewise order, strictly after an exclusive cursor, and use the last returned key as
the next cursor. `MemoryStore` and `FileStore` implement this bounded path; the sender
falls back to `BlobKeyLister.ListKeys` for older stores. A page may reflect keys added
or removed between calls, so the periodic inventory pass remains the convergence
mechanism rather than a snapshot guarantee. A key added at or before the live cursor can
wait for that next pass; `-sync-interval 0s` is therefore startup-only and does not promise
snapshot-consistent convergence under concurrent mutation. See
[INVENTORY_CONSISTENCY.md](INVENTORY_CONSISTENCY.md) for the mutation matrix and acceptance
evidence.

The CLI scheduler keeps one cursor per peer across a bounded exchange. The aggregate
`-max-inventory-bytes` budget defaults to 16 MiB of encoded inventory payload and
`-max-inventory-keys` defaults to 16,384 advertised keys; `0` disables the corresponding
cap. A saturated exchange continues after a short delay from the last exclusive cursor.
This is scheduler state, not a new wire message: mixed-version peers still receive
ordinary `blob.has` frames. A minimum frame is sent even when a deliberately tiny byte
budget cannot fit one key, so the cursor can always advance.

## Limits

Default replication limits are:

| Limit | Value | Error |
|-------|-------|-------|
| Max key size | 512 bytes | `replication.ErrKeyTooLarge` |
| Max keys per inventory message | 4096 | `replication.ErrTooManyKeys` |
| Max inventory bytes per peer exchange | 16 MiB by default; `-max-inventory-bytes`, `0` unlimited | Scheduler continues from the exclusive cursor |
| Max inventory keys per peer exchange | 16,384 by default; `-max-inventory-keys`, `0` unlimited | Scheduler continues from the exclusive cursor |
| Max blob payload | `4 << 20` bytes | `replication.ErrDataTooLarge` |
| Max repair data per `blob.missing` response | `64 << 20` bytes | Remaining keys are deferred |
| Max peer auth payload | `4 << 10` bytes | `p2p.ErrPeerAuthPayloadTooLarge` |
| Max peer auth capabilities | 16 entries, 512 aggregate bytes, 64 bytes per entry | `p2p` capability validation errors |
| Max lifecycle record | `64 << 10` bytes | `lifecycle.ErrRecordTooLarge` |
| Max lifecycle envelope | `128 << 10` bytes | `lifecycle.ErrLifecyclePayloadTooLarge` |
| Max lifecycle repair batch/snapshot | 128 records, 64 KiB logical bytes, 64 KiB metadata, 128 KiB encoded payload | `lifecycle.ErrRepairLimit` / `lifecycle.ErrRepairFrameTooLarge` |
| Max durable repair peers | 1,024 peers, 128 bytes per identity, 1 MiB encoded state | `lifecycle.ErrWatermarkLimit` / `lifecycle.ErrWatermarkPeerInvalid` |

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

Malformed frames are rejected before payload allocation when their magic or declared
length is invalid. Malformed JSON, base64 fields, unknown message types, empty keys,
oversized keys/data, and overfull key lists are rejected by `replication.Decode` with a
validation error; they are not applied to storage. The bounded `FuzzReadFrame`,
`FuzzDecode`, and round-trip fuzz targets exercise these boundaries in the CI
`protocol-fuzz` smoke job, while longer fuzzing remains a manual or scheduled activity.

Lifecycle envelopes use their own bounded decoder. An oversized lifecycle payload is refused
before JSON decoding; a valid envelope still has to pass the inner lifecycle record limits.
Unknown lifecycle types and malformed or trailing JSON are rejected without applying a record.

When answering `blob.missing` or `blob.get`, StreamHive treats each requested key as an
independent send unit until bytes are written to the peer. If one key is unreadable or
cannot be encoded under the configured limits, that key is skipped, `replication_send_errors`
and `replication_blobs_skipped` are incremented, and later requested keys are still sent.
If writing a frame to the TCP stream fails, the peer loop stops instead of retrying the
same frame on a potentially partial stream.
For anti-entropy `blob.missing` responses, the sender also bounds aggregate blob data per
request with `MaxRepairBytes`. Once that budget is reached, the remaining requested keys
are deferred, the connection stays healthy, and `replication_repair_blobs_deferred` is
incremented. The sender then schedules one delayed continuation per peer, merging duplicate
requests and capping pending keys at `MaxKeys`; pending work is dropped on disconnect or
shutdown. A later periodic inventory or reconnect remains the fallback for keys that are
still deferred or arrive after the bounded continuation. The first blob is allowed through
when it alone exceeds a deliberately smaller budget so repair cannot be permanently wedged;
operators should keep `MaxRepairBytes` at or above the maximum blob size when they need a
strict aggregate cap.
Each anti-entropy blob read and frame write also acquires the process-wide
`-max-repair-ops` budget (default four); queued and canceled operations are visible through
the aggregate `replication_repair_io_ops_*` metrics without peer or blob labels.

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

Repair is driven by startup inventory, one bounded delayed continuation after a repair
budget hit, periodic inventory with `-sync-interval`, and reconnect behavior for static
`-peers` when `-peer-reconnect` is enabled. If a blob send fails mid-sync, a later inventory
pass or reconnect can request the missing key again.
When a store contains more keys than one inventory message permits, the owner emits
multiple bounded `blob.has` frames at the configured `MaxKeys` limit instead of failing
the entire advertisement. Each frame must also fit the transport `MaxFrameBytes` limit;
the sender adaptively splits a batch further when necessary, while a single key that
cannot fit fails the inventory pass with a frame-size error. The per-peer scheduler also
stops an exchange when its aggregate byte or key budget is reached, records the last key,
and resumes later without changing the frame format. Startup and periodic triggers are
coalesced, so one peer cannot start overlapping inventory exchanges.
The receiver checks each advertised key with its `BlobStore.Has` operation, so the work
and temporary key set are bounded by the incoming inventory frame rather than the full
local store size.
Aggregate anti-entropy counters make that control loop visible without peer labels:
`replication_inventory_advertisements` counts successful `blob.has` frames,
`replication_inventory_bytes_sent` counts their encoded payload bytes, and
`replication_inventory_keys_sent` counts advertised keys,
`replication_inventory_keys_probed` counts receiver-side `BlobStore.Has` probes for
advertised keys. `replication_inventory_exchanges_started`,
`replication_inventory_exchanges_completed`, `replication_inventory_exchanges_limited`,
and `replication_inventory_exchanges_dropped` count scheduler outcomes, while
`replication_inventory_exchanges_active` counts current per-peer entries. These counters
have no peer, blob, or key labels. `replication_missing_keys_requested` counts keys in
`blob.missing` messages, and
`replication_repair_blobs_sent` counts successful repair `blob.put` frames, while
`replication_repair_blobs_deferred` counts requested keys held back by the per-response
repair budget. `replication_corrupt_blobs_detected` counts damaged content-addressed
files observed while accepting a verified repair and replacing the bad bytes.
`replication_repair_continuations_scheduled`,
`replication_repair_continuations_completed`, and
`replication_repair_continuations_dropped` count delayed continuation batches without peer
labels. `replication_repair_continuations_active` counts scheduled or running per-peer
continuation entries, and `replication_repair_continuation_keys_pending` counts queued
plus in-flight keys. These gauges expose repair saturation without peer or blob labels
and return to zero after completion, disconnect, or shutdown. Repair
delivery logs use `delivery=anti-entropy`; one-shot CLI deliveries use
`delivery=one-shot`. Requested-blob delivery checks cancellation between store reads,
encoding, and frame writes, so shutdown stops a long repair pass at the next safe
boundary. Ordinary missing or unreadable blobs remain skip-and-continue cases and can
be requested again by a later inventory pass.

The inventory scaling decision and reproducible flat-vs-digest measurements are recorded
in [ANTI_ENTROPY.md](ANTI_ENTROPY.md). The current decision is to keep the bounded flat
`blob.has` protocol with aggregate per-peer exchange budgets until a real workload
demonstrates that its frame, CPU, or continuation limits are insufficient; the aggregate
inventory counters make whole-exchange pressure visible before that decision.

## Observability

Use `/metrics` for JSON counters, `/metrics/prometheus` for Prometheus text format, and
`/peers` for connected peer metadata. `/metrics` includes peer auth, transport, and
replication counters, including `peer_auth_identity_rejections` for inbound identity
allowlist failures and the aggregate anti-entropy counters described above. `/peers`
includes remote address, local address, direction,
connection timestamp, connection age in milliseconds, `auth_method` (`none` or
`shared-token`), and optional `auth_identity`.

The Prometheus endpoint emits a sorted, label-free family for every JSON metric. Each family
has exactly one `# HELP`, one `# TYPE`, and one sample line; metric names and sample values are
identical to the JSON keys and values after the `streamhive_` prefix is added. Existing names are
kept for compatibility even when a counter name does not end in `_total`. Transport and
replication events are `counter` families. Active, pending, in-flight, queued, and TLS
credential-health values are `gauge` families, including `active_peers`,
`replication_blob_acks_pending`, inventory/repair active or pending values, repair I/O in-flight
or queued values, and the `tls_certificate_*` / `tls_certificates_*` values. There are no labels,
exemplars, peer addresses, blob keys, or certificate metadata.

The optional HTTP health server also uses fixed bounds rather than unbounded request handling:
5-second header reads, 10-second request reads, 10-second response writes, 60-second idle
connections, and 1 MiB maximum headers. Process cancellation gracefully shuts down the server;
the P2P wire protocol and endpoint paths are unchanged.

## Shutdown and Drain

There is no shutdown message in the StreamHive wire protocol. The current transport
closes its listener, cancels local handlers, and closes the tracked TCP peers; a
remote peer observes an ordinary EOF or connection error. A close does not
acknowledge or guarantee delivery of an in-flight blob. Reconnect and anti-entropy
repair remain the recovery paths for work interrupted by shutdown.

The v0.12 transport implements a local, staged, caller-bounded peer drain through
`TCPTransport.Drain(ctx)`, with no new frame type or mixed-version handshake.
`Close()` remains the immediate hard-stop path. Both preserve SHV1 framing and the
existing replication messages. See [PEER_DRAIN.md](PEER_DRAIN.md) for the lifecycle
contract, deadline semantics, and acceptance evidence.
