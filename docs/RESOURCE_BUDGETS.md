# Resource Budgets

This note records the v0.12 operational envelope for one StreamHive process. It is
an implementation baseline, not a promise that the current defaults are suitable for
every deployment. Operators should set a finite `-max-peers` value before exposing a
node to untrusted or unexpectedly large peer sets.

## Decision

Keep the current wire protocol and its per-message limits. The next safety boundary is
process-level admission and I/O scheduling, not another message type or digest format.
The current implementation already bounds a single frame, key list, blob, repair
response, per-peer continuation queue, process-wide repair I/O operation count, native
store inventory page, and aggregate inventory exchange. Native stores page a large
inventory before wire batching; older `BlobKeyLister` implementations remain supported
through a compatibility fallback.
MemoryStore and FileStore keep process-local ordered key indexes; FileStore rebuilds its
index from durable filenames when first used or when the directory modification stamp
changes.

This split follows the same operational shape used by bounded distributed systems:
request size and storage quotas are explicit separately in etcd, while memberlist uses
bounded periodic exchange rather than assuming one state transfer is sufficient. The
relevant references are [etcd system limits](https://etcd.io/docs/v3.7/dev-guide/limit/),
[memberlist state exchange](https://github.com/hashicorp/memberlist/blob/master/state.go),
and [memberlist configuration](https://github.com/hashicorp/memberlist/blob/master/config.go).

## Current Budget Matrix

| Resource | Setting and default | Owner | Saturation or failure mode |
| --- | --- | --- | --- |
| Admitted peers | `-max-peers`; `0` means unlimited | `p2p.TCPTransport` | New peers are closed before registration; `peers_rejected` increments. Set a finite value in production. |
| One frame payload | `p2p.DefaultMaxFrameBytes`: 4 MiB; override `TCPTransport.MaxFrameBytes` | `p2p.ReadFrame` / `WriteFrame` | Declared oversized frames are rejected before payload allocation; the peer loop exits. |
| One key | `replication.DefaultMaxKeyBytes`: 512 bytes | `replication.Decode` | The message is rejected with `ErrKeyTooLarge`. |
| One inventory message | `replication.DefaultMaxKeys`: 4,096 keys | `replication.Decode` and repair scheduler | The message is rejected or pending continuation keys are deduplicated and dropped at the cap. |
| One blob | `replication.DefaultMaxDataBytes`: 4 MiB; CLI `-max-blob-bytes` | `replication.Decode` | The message is rejected with `ErrDataTooLarge` before storage. |
| One repair response | `replication.DefaultMaxRepairBytes`: 64 MiB; CLI `-max-repair-bytes` | `sendRequestedBlobsResult` | Remaining keys are deferred to one delayed continuation and later inventory passes. |
| One inventory page | `BlobKeyPager` requested limit; direct non-positive limits use `storage.DefaultKeyPageSize`: 256 | `MemoryStore`, `FileStore`, and `sendBlobHas` | Both native stores seek an ordered B-tree and retain one page. FileStore rebuilds its process-local index from `File.ReadDir(128)` chunks when the directory stamp changes. Legacy `BlobKeyLister` stores still use their complete-list API. |
| One inventory exchange | `-max-inventory-bytes`: 16 MiB default, `0` unlimited; `-max-inventory-keys`: 16,384 default, `0` unlimited; `MaxKeys`: 4,096 per frame; transport `MaxFrameBytes`: 4 MiB by default | Per-peer inventory exchange scheduler and `blob.has` receiver | The scheduler retains one exclusive cursor per peer, sends a bounded chunk, and schedules a delayed continuation on budget saturation. Startup and periodic triggers are coalesced; disconnect or shutdown drops the cursor. A tiny byte budget still permits one minimum frame for progress. |
| Global repair I/O operations | CLI `-max-repair-ops`; default 4, `0` selects the default | `repairIOLimiter` | Each anti-entropy blob read/write waits for a permit; cancellation rejects the waiter. `repair_io_ops_*` metrics show pressure without labels. |
| Per-peer repair queue | One running continuation and at most `MaxKeys` pending keys per peer | `repairContinuationScheduler` | New unique keys beyond the queue cap are dropped and counted; disconnect or shutdown discards the entry. |
| Reconnect delay | 500 ms minimum, 30 s maximum by CLI defaults | `peerReconnector` | Failed static targets back off exponentially and stop on context cancellation. |
| Static reconnect loops | One retry loop per unique `-peers` target; target count is operator-configured | `peerReconnector` | Duplicate schedules coalesce per target; `peer_reconnect_active` exposes live dialing/backoff loops, while attempts, non-shutdown failures, and successes remain aggregate. |
| TLS handshake | `DefaultTLSHandshakeTimeout`: 5 s; library override `TLSHandshakeTimeout` | `p2p.TCPTransport` | Inbound certificate verification completes before peer registration; timeout or certificate failure closes the connection and increments aggregate TLS failure metrics. |
| Auth handshake | 5 s default timeout, 128-byte identity limit | `p2p` handshake | Invalid or late auth closes the connection before peer registration. |

`replication_inventory_exchanges_dropped` counts canceled or failed inventory scheduler
work without peer labels.

The continuation and repair-I/O gauges are intentionally aggregate. With `P` admitted peers, the
current scheduler has at most `P` active entries and at most `2 * P * MaxKeys` keys in
the pending gauge because it includes one in-flight batch plus a newly queued batch per
peer. The bound is useful only when `P` is finite. Prometheus recommends exposing
in-progress and queued work while avoiding high-cardinality labels, which is why the
gauges contain no peer address or blob key. See the [Prometheus instrumentation
guidance](https://prometheus.io/docs/practices/instrumentation/) and [label naming
guidance](https://prometheus.io/docs/practices/naming/).

## Measured Saturation

The executable acceptance tests cover two hard saturation points. The continuation test
schedules `DefaultMaxKeys + 1` distinct deferred keys for one blocked peer:

```text
queue saturation: max_keys=4096 requested=4097 admitted=4096 dropped_events=1 active=1 pending=4096
```

Run it directly or through the CI target:

```bash
go test -race . -run '^TestRepairContinuationSchedulerSaturatesAtConfiguredKeyBudget$' -count=1 -v
make test-budgets
```

This is a queue-admission measurement, not a throughput claim. The global limiter test
uses `max_ops=1`, holds the only permit with one peer, queues a healthy peer, and
verifies that the healthy operation completes after release. This is an admission and
progress measurement, not a throughput claim. Existing frame, storage, anti-entropy,
fairness, and Docker demos should be read alongside it for latency and recovery
behavior. The inventory benchmark is intentionally a scaling and startup-budget
measurement. On one Apple M1 sample, MemoryStore took about 0.095 ms and 131 KB for a
4,096-key full listing versus 0.092 ms and 144 KB for indexed pages; at 65,536 keys the
figures were 1.54 ms and 2.54 MB versus 1.80 ms and 2.65 MB. FileStore took about
0.089 ms and 164 KB versus 0.133 ms and 182 KB at 4,096 keys, and 1.76 ms and 2.62 MB
versus 7.57 ms and 2.82 MB at 65,536 keys. Its lazy index build measured about 3.2 ms
and 0.85 MB at 4,096 keys, and 75.1 ms and 13.3 MB at 65,536 keys. The pre-index
MemoryStore repeated-scan page path took about 824.5 ms and 9.4 MB at 65,536 keys.
These are local signals, not portable latency promises; the rebuild budget is explicit
and the durable file format remains unchanged.

Run the comparison with:

```bash
make bench-inventory
```

The end-to-end inventory benchmark measures both the unbroken flat exchange and the
default aggregate budgets. The local Apple M1 samples below are approximate and are
scaling signals, not service-level promises:

| Key bytes | Keys | Mode | Chunks | Frames | Wire bytes | Time | Allocations/op |
|---:|---:|---|---:|---:|---:|---:|---:|
| 32 | 65,536 | Flat | 1 | 16 | 3.08 MB | 27.8 ms | 30.8 MB |
| 32 | 65,536 | Budgeted | 5 | 16 | 3.08 MB | 5.7 ms | 14.4 MB |
| 512 | 65,536 | Flat | 1 | 16 | 45.0 MB | 290 ms | 320 MB |
| 512 | 65,536 | Budgeted | 5 | 16 | 45.0 MB | 49 ms | 175 MB |

The budgeted rows use the default 16 MiB and 16,384-key exchange limits. Five chunks
means four data chunks plus a final empty cursor check that proves the exchange is
complete. Wire volume and receiver probes remain the same because the flat protocol is
unchanged, while each native pager call holds only one bounded page at a time.

## Pressure Behavior

- **Peer pressure:** configure `-max-peers`; otherwise admission is intentionally
  unlimited for backwards-compatible local demos.
- **Repair pressure:** each peer progresses independently. A slow write delays that
  peer's continuation only, and bounded pending work is observable through aggregate
  gauges and drop counters.
- **Storage pressure:** individual reads and writes are sequential within a repair
  response, while `-max-repair-ops` caps aggregate anti-entropy read/write operations
  across peers. Waiters observe `replication_repair_io_ops_queued`, and canceled waiters
  increment `replication_repair_io_ops_rejected`.
- **Inventory pressure:** native `BlobKeyPager` stores enumerate only a bounded page
  before outbound `blob.has` frame paging. Both native stores seek through ordered
  indexes; FileStore pays a one-time O(N) key/index rebuild after startup or an external
  directory mutation. The per-peer scheduler now bounds each exchange by aggregate
  encoded bytes and advertised keys, retaining one exclusive cursor for delayed
  continuation. `replication_inventory_bytes_sent`, `replication_inventory_keys_sent`,
  `replication_inventory_exchanges_limited`, and `replication_inventory_exchanges_active`
  expose that pressure without labels. Legacy `BlobKeyLister` implementations may still
  materialize the complete store and should be migrated when their ownership is known.
- **Shutdown:** transport shutdown cancels peer handlers; the scheduler forgets pending
  continuations and leaves the gauges at zero. In-flight file operations observe the
  shared context between blob operations.
- **Reconnect:** reconnect retries are bounded by the configured backoff, while the
  next inventory exchange remains the repair fallback after a disconnect. The retry loop is one
  per unique configured target; `peer_reconnect_targets`, `peer_reconnect_active`,
  `peer_reconnect_attempts`, `peer_reconnect_failures`, and `peer_reconnect_successes` expose
  aggregate pressure and recovery without target labels. Cancellation during shutdown removes
  active loops and is not counted as a failure.

## Follow-up Work

1. Revisit a safe finite default for `-max-peers` after the representative workload
   and deployment topology are defined. Until then, production operators should set it
   explicitly and alert on `peers_rejected`.
2. Revisit a durable FileStore manifest only after a workload justifies its crash,
   rebuild, and multi-process ownership rules; the current process-local index keeps
   the durable hex-file format small and recoverable.

These changes are deliberately separate from the wire protocol and can be implemented
independently.

Peer admission details and the real-TCP saturation proof are documented in
[PEER_ADMISSION.md](PEER_ADMISSION.md). The v0.12 decision keeps `-max-peers 0` unlimited for
compatibility while requiring a finite setting for production deployments.
