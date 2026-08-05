# Resource Budgets

This note records the v0.12 operational envelope for one StreamHive process. It is
an implementation baseline, not a promise that the current defaults are suitable for
every deployment. Operators should set a finite `-max-peers` value before exposing a
node to untrusted or unexpectedly large peer sets.

## Decision

Keep the current wire protocol and its per-message limits. The next safety boundary is
process-level admission and I/O scheduling, not another message type or digest format.
The current implementation already bounds a single frame, key list, blob, repair
response, and per-peer continuation queue. It does not yet enforce a process-wide
repair concurrency budget or stream a large store inventory, so those are follow-up
implementation tasks rather than hidden assumptions.

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
| Per-peer repair queue | One running continuation and at most `MaxKeys` pending keys per peer | `repairContinuationScheduler` | New unique keys beyond the queue cap are dropped and counted; disconnect or shutdown discards the entry. |
| Reconnect delay | 500 ms minimum, 30 s maximum by CLI defaults | `peerReconnector` | Failed static targets back off exponentially and stop on context cancellation. |
| Auth handshake | 5 s default timeout, 128-byte identity limit | `p2p` handshake | Invalid or late auth closes the connection before peer registration. |

The continuation gauges are intentionally aggregate. With `P` admitted peers, the
current scheduler has at most `P` active entries and at most `2 * P * MaxKeys` keys in
the pending gauge because it includes one in-flight batch plus a newly queued batch per
peer. The bound is useful only when `P` is finite. Prometheus recommends exposing
in-progress and queued work while avoiding high-cardinality labels, which is why the
gauges contain no peer address or blob key. See the [Prometheus instrumentation
guidance](https://prometheus.io/docs/practices/instrumentation/) and [label naming
guidance](https://prometheus.io/docs/practices/naming/).

## Measured Saturation

The executable acceptance test schedules `DefaultMaxKeys + 1` distinct deferred keys
for one blocked continuation peer. It records the first hard saturation point:

```text
queue saturation: max_keys=4096 requested=4097 admitted=4096 dropped_events=1 active=1 pending=4096
```

Run it directly or through the CI target:

```bash
go test -race . -run '^TestRepairContinuationSchedulerSaturatesAtConfiguredKeyBudget$' -count=1 -v
make test-budgets
```

This is a queue-admission measurement, not a throughput claim. Existing frame,
storage, anti-entropy, fairness, and Docker demos should be read alongside it for
latency and recovery behavior. The first process-level saturation point remains
unknown because the current implementation has no global repair semaphore and
`BlobKeyLister.ListKeys` returns a complete inventory slice. Both gaps are tracked as
follow-up issues below.

## Pressure Behavior

- **Peer pressure:** configure `-max-peers`; otherwise admission is intentionally
  unlimited for backwards-compatible local demos.
- **Repair pressure:** each peer progresses independently. A slow write delays that
  peer's continuation only, and bounded pending work is observable through aggregate
  gauges and drop counters.
- **Storage pressure:** individual reads and writes are sequential within a repair
  response, but all admitted peer sessions may perform storage work concurrently. There
  is no global read/write semaphore yet.
- **Inventory pressure:** each outbound `blob.has` frame is bounded by key and frame
  limits, but `ListKeys` can materialize the entire store before paging the wire
  messages. Large stores therefore need an operator-level store-size and process-memory
  envelope until pagination exists.
- **Shutdown:** transport shutdown cancels peer handlers; the scheduler forgets pending
  continuations and leaves the gauges at zero. In-flight file operations observe the
  shared context between blob operations.
- **Reconnect:** reconnect retries are bounded by the configured backoff, while the
  next inventory exchange remains the repair fallback after a disconnect.

## Follow-up Work

1. Add a global repair scheduler budget for total concurrent reads/writes and expose
   aggregate admission, in-flight, wait, and rejection counters without peer labels.
2. Add a paginated or iterator-based inventory API so store enumeration does not
   require a complete key slice in memory.
3. Revisit a safe finite default for `-max-peers` after the representative workload
   and deployment topology are defined. Until then, production operators should set it
   explicitly and alert on `peers_rejected`.

The first two changes are deliberately separate from the wire protocol and can be
implemented independently.
