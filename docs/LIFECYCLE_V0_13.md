# v0.13 Versioned Lifecycle Semantics

Status: v0.13.0 design plus the shipped capability, record-transport, apply, internal repair,
capability-gated repair-frame, and caller-owned repair-session boundaries from issues #51,
#52, #53, #54, and #55. This document extends the released v0.12.0 add-only blob contract;
opt-in CLI scheduling is shipped, while compaction operations, membership administration, and
raw blob deletion remain future slices.

## Problem Statement

StreamHive has two different concepts that must remain separate:

1. The raw blob namespace maps a 32-byte SHA-256 key to immutable bytes. A missing raw blob
   may be repaired from a peer because absence is local storage state.
2. A future logical namespace maps an application key to a present or deleted lifecycle
   record. A logical delete must participate in repair or an old replica can resurrect the
   object.

`BlobStore.Delete` remains local eviction or garbage collection in v0.12. It is not a
distributed delete. The v0.13 design below adds a bounded logical namespace later, without
changing the meaning of raw blob operations.

## Transport Slice Shipped

Issue #51 adds the first wire boundary without enabling lifecycle synchronization:

- `p2p.TCPTransport.PeerAuthCapabilities` advertises the bounded `lifecycle.v1` capability in
  the existing authenticated envelope.
- Unknown incoming capabilities are ignored for forward compatibility; duplicate, invalid, or
  oversized declarations are rejected deterministically.
- `TCPPeer.AuthCapabilities`, `PeerSnapshot.Capabilities`, and `/peers` expose the negotiated
  set. `ready`, `optional-raw-only`, and `required-unavailable` make mixed-version readiness
  explicit without adding metric labels.
- `internal/lifecycle.EncodeRecord` and `DecodeRecord` bound and validate one
  `lifecycle.record` envelope independently from raw blob limits.
- `internal/lifecycle.Applier` refuses peers without `lifecycle.v1`, verifies present-record
  bytes before raw storage and journal publication, and applies deletes without raw deletion.
- No lifecycle frame is sent to a peer without negotiated `lifecycle.v1`; ordinary `blob.*`
  traffic is unchanged. The caller-owned repair session is the first sender/receiver boundary;
  CLI scheduling is opt-in through the lifecycle flags and remains disabled by default.

## Decision

The first lifecycle implementation uses an **operator-fenced single authority per namespace**.

- Each logical namespace has one configured authority identity. The authority serializes all
  logical puts and deletes for that namespace.
- A mutation receives a durable version token `(epoch, sequence)`. `epoch` changes when an
  operator deliberately reassigns the authority; `sequence` increases for every mutation in
  that epoch and resumes from durable state after restart.
- The current authority is an operational trust boundary, not an automatic consensus system.
  TLS/mTLS and the existing identity allowlist can authenticate the peer, while deployment
  policy must prevent two writers from using the same authority epoch.
- Replicas apply newer tokens, treat exact duplicates as idempotent, ignore older tokens, and
  reject a same-token record whose body differs from the stored body.
- Multi-writer version sets, automatic leader election, and automatic authority failover are
  out of scope for the first implementation.

This gives StreamHive a total order and a bounded catch-up cursor without pretending that a
shared-token identity label is consensus. An operator can recover a failed authority by fencing
the old deployment and assigning a higher epoch; a live old writer must be fenced before the
new epoch is used.

## Lifecycle Record

The future logical record is separate from the immutable blob store:

```text
LifecycleRecord {
    namespace_id  bounded bytes
    logical_key   bounded bytes
    state         present | deleted
    blob_key      optional 32-byte SHA-256 key
    version       { epoch uint64, sequence uint64 }
    authority_id  bounded printable identity
}
```

Rules:

- A `present` record must reference an immutable blob key. The referenced bytes must pass
  SHA-256 verification before the logical record becomes visible.
- A `deleted` record has no blob payload. The old immutable blob may remain physically stored
  until it is unreferenced and lifecycle retention permits garbage collection.
- A raw `blob.put` received from a v0.12 or lifecycle-unaware peer never creates or resurrects
  a logical record. It only makes immutable bytes available to the raw namespace.
- The authority persists the record before acknowledging the logical mutation. A replica may
  expose a record only after its lifecycle metadata and, for `present`, its verified blob are
  durable.
- The logical key is application data and must have an explicit byte limit. It is not silently
  treated as a content hash and must not become a metric label.

## Ordering And Conflict Rules

Tokens compare lexicographically by `(epoch, sequence)`.

| Incoming record | Result |
| --- | --- |
| Newer token than local state | Validate and apply |
| Older token | Ignore as stale; count the outcome |
| Same token and identical body | No-op duplicate |
| Same token and different body | Reject and raise a lifecycle conflict |
| Present record with missing or invalid blob | Keep pending or reject; never publish it |
| Delete record | Apply the tombstone without deleting raw bytes |

The authority must fail readiness rather than reuse a sequence after journal corruption or an
ambiguous recovery. A new epoch is the only supported way to move authority ownership. A
per-key revision alone is insufficient for the first implementation: it can order updates to one
key but does not provide a compact global journal watermark for catch-up and safe compaction.

Concurrent writes from different authorities are not merged. They are rejected until a future
multi-writer design defines causal context and an application conflict policy.

## Durable Journal And Crash Ordering

Lifecycle metadata uses a separate versioned sidecar or journal; the v0.12 raw blob file layout
is unchanged.

For a present record, the safe order is:

1. receive or read the immutable blob;
2. write it to a temporary file, fsync, and atomically rename it into the raw store;
3. append the lifecycle record to the journal and fsync the journal;
4. publish the new logical view and acknowledge the mutation.

For a delete, append and fsync the tombstone before publishing the deleted view. A crash may
leave an unreferenced blob, but it must not leave a visible record pointing at unverifiable
bytes. A truncated final journal entry is discarded during recovery; a checksum or length
envelope must make the truncation detectable.

Snapshots/checkpoints are written to a temporary file, fsynced, and atomically renamed. Journal
truncation is allowed only after the checkpoint and its compaction watermark are durable.
Recovery replays the checkpoint tail and fails readiness on an invalid non-tail record rather
than silently skipping lifecycle history.

## Replication, Cursors, And Repair

Lifecycle replication must not reuse the raw `BlobKeyPager` live cursor. The raw blob inventory
continues to be a live, bounded, periodic repair mechanism as documented in
`INVENTORY_CONSISTENCY.md`.

The future lifecycle stream uses:

- an ordered mutation journal keyed by `(epoch, sequence)`;
- one acknowledged lifecycle watermark per configured peer;
- bounded batches by entry count, logical-key bytes, metadata bytes, and frame bytes;
- a bootstrap snapshot containing the current logical state, including tombstones, followed by
  journal entries after the snapshot watermark;
- a full snapshot fallback when a peer's watermark is older than the retained journal floor.

For a `present` record, the receiver validates the record, obtains the referenced blob through
the existing bounded blob path, verifies the SHA-256 key, durably stores it, then applies the
record. A delete does not wait for blob transfer. If the journal or snapshot is too old to repair
a peer and no safe snapshot is available, the peer is lifecycle-unready and must be reseeded;
the system must not silently claim convergence.

### Internal Repair Boundary Shipped

Issue #53 adds reusable repair state without changing `SHV1`, the raw `blob.*` messages, or the
CLI. `internal/lifecycle.RepairBatch` selects a strictly ordered, bounded journal prefix and
`RepairBatch.Delivery` classifies an exact replay as a duplicate while refusing gaps and
reordered records. `RepairSnapshot` and `PlanRepair` provide a complete checkpoint fallback when
the peer watermark is older than the retained journal floor; a missing checkpoint returns an
explicit snapshot-required error.

`WatermarkBook` stores per-peer acknowledgements in a checksummed envelope through an atomic
fsynced rename. Acknowledgements are monotonic, bounded by peer count, identity size, and file
size, and reload across process restart. `RepairCoordinator` reads the durable watermark for a
peer, resumes the next batch after reconnect, and rejects acknowledgements beyond the local
journal tail. `RepairFrame` gives callers one bounded union for batch, snapshot, and watermark
ack payloads, while `DecodeRepairFrameForPeer` refuses lifecycle data before decoding unless the
caller supplies the negotiated `lifecycle.v1` capability. `RepairSession` composes those pieces:
`SendNext` plans and writes one bounded frame, while `Handle` validates, applies, and acknowledges
received batches or snapshots only after durable apply. It refuses gaps, preserves duplicate
replay safety, and leaves the durable watermark ready for reconnect or process restart. The CLI
constructs one cancellable session per authenticated lifecycle-capable peer only when `-lifecycle`
is enabled; its aggregate readiness and repair counters are exposed without peer labels. The
default CLI path remains raw-only, while membership and compaction controls remain later work.

The authority keeps tombstones until every configured lifecycle replica has acknowledged a
version at or beyond the tombstone and a durable checkpoint includes it. Unknown or removed
members block automatic compaction until an operator creates a new membership epoch and fences
the old member. Wall-clock age alone is never sufficient to discard a tombstone.

Physical blob garbage collection is a separate step. It requires that no current lifecycle
record references the blob and that the tombstone/journal retention proof is complete. A shared
blob referenced by multiple logical keys is retained until all references are gone.

The mutation journal must have bounded bytes, entries, and compaction concurrency. If the journal
reaches its configured safety limit and cannot compact safely, new lifecycle mutations fail
closed with an observable error; history is never dropped to preserve availability.

## Option Comparison

| Model | Decision | Reason |
| --- | --- | --- |
| Local eviction only | Keep for v0.12 raw blobs | Safe and simple, but it does not express a logical delete |
| Single authority plus epoch/sequence | Select for first v0.13 implementation | Gives total order, bounded watermarks, and a small conflict surface |
| Per-key revision only | Reject as the complete model | Orders one key but does not provide a global journal/catch-up watermark |
| Multi-writer version set or vector clock | Defer | Requires causal metadata, bounded context growth, and application conflict resolution |
| Automatic leader election | Defer | Requires consensus, fencing, membership, and recovery semantics beyond StreamHive's current transport |
| TTL expiry | Defer | Clock skew and expiry are deletes; they still need authority ordering and tombstone retention |
| Automatic raw-blob-to-logical-key migration | Reject | A content hash does not identify an application's logical key or desired lifecycle state |

## Mixed-Version And Rollback Contract

The lifecycle capability is negotiated before lifecycle records are exchanged. A v0.12 peer does
not understand lifecycle records and remains on ordinary `blob.has`, `blob.missing`, `blob.get`,
and `blob.put` behavior. It never receives an unknown lifecycle frame.

If a logical namespace is configured as required, a connection without the lifecycle capability
is not lifecycle-ready and the namespace reports degraded readiness. If lifecycle state is
optional, raw blob replication may continue while logical synchronization is explicitly marked
unavailable. Neither mode silently turns a raw eviction into a logical delete.

Downgrading to v0.12 preserves raw blobs and leaves the lifecycle sidecar untouched. The older
binary ignores the sidecar and continues add-only behavior; lifecycle reads, compaction, and
logical garbage collection must remain disabled until a lifecycle-capable version returns.
There is no automatic rollback that converts logical deletes into raw `BlobStore.Delete` calls.

## Observability And Readiness

Metrics remain aggregate and label-free. The opt-in CLI runtime currently exposes lifecycle
enablement, active/started/completed repair sessions, received frames, and frame/session errors
through JSON and Prometheus health endpoints. The full lifecycle observability target remains:

- counters for lifecycle records applied, duplicates, stale records, conflicts, capability
  refusals, invalid blob references, snapshot bootstraps, journal repairs, and compactions;
- gauges for lifecycle readiness, pending records, journal bytes, tombstone count, minimum peer
  watermark, and compaction-blocked state;
- bounded logs with namespace, version token, operation, outcome, and reason, without logical
  keys, blob contents, certificate data, or remote-address metric labels.

`/readyz` remains healthy for the existing v0.12 raw namespace. A lifecycle-required namespace
is not ready when its journal cannot be recovered, its authority is invalid, a required peer is
behind the retained journal floor, or a safe compaction/repair limit has been exceeded. These
states must be distinguishable from ordinary raw blob availability.

## Test And Implementation Split

The research decision is complete only when the future implementation is split into independently
verifiable slices:

1. **Model and storage:** token ordering, idempotency, stale rejection, journal checksums,
   truncated-tail recovery, atomic checkpoints, and compaction watermarks.
2. **Capability and record transport:** bounded lifecycle negotiation, record encoding limits,
   explicit v0.12 refusal, and no admission of unknown frames. This boundary is shipped by
   issue #51.
3. **Apply path:** verified blob-before-record ordering, delete application, duplicate replay,
   partial-write recovery, and raw-blob non-resurrection. The reusable capability-gated apply
   path is shipped by issue #52; wire repair and operations remain separate.
4. **Repair:** per-peer watermarks, bounded journal batches, snapshot bootstrap, behind-floor
   reseeding, reconnect, partition, and restart acceptance. The reusable internal planning and
   durable-watermark boundary is shipped by issue #53; the capability-gated frame codec and
   real-TCP compatibility boundary are shipped by issue #54; automatic network scheduling and
   operations remain separate.
5. **Operations:** CLI configuration, readiness states, aggregate JSON/Prometheus metrics,
   compaction controls, migration/rollback runbook, and a deterministic multi-node demo.

The deterministic test matrix must include put/delete/reconnect/restart in every order, a stale
peer returning after compaction, duplicate and reordered records, same-token conflicts, concurrent
raw blob arrival, corrupt or missing referenced blobs, journal truncation, failed checkpoint
renames, membership removal, and a v0.12 mixed-version peer. Property tests should assert that
reordering and duplicate delivery cannot produce a different state from token order.

## Non-Goals

- The research issue #49 made no wire, capability, CLI, or storage-format change; issue #51
  adds only the negotiated transport boundary and internal record codec.
- No automatic consensus, leader election, multi-writer merge, TTL expiry, or conflict resolver
  is promised by v0.13's first lifecycle slice.
- No physical raw-blob deletion is implied by a logical tombstone.
- The existing v0.12 live inventory cursor is not upgraded into a snapshot or revision stream.

## Research References

- [Dynamo: Amazon's highly available key-value store](https://www.amazon.science/publications/dynamo-amazons-highly-available-key-value-store)
  motivates explicit version context and application conflict handling for multi-writer systems.
- [etcd data model](https://etcd.io/docs/v3.7/learning/data_model/) documents revisions,
  tombstones, generations, and compaction as an integrated MVCC lifecycle model.
- [Apache Cassandra tombstones](https://cassandra.apache.org/doc/latest/cassandra/managing/operating/compaction/tombstones.html)
  demonstrates why deletion markers must survive repair and why age-only removal can resurrect
  deleted data on a long-disconnected replica.
- [Raft](https://raft.github.io/raft.pdf) is the reference point for the consensus and fencing
  complexity deliberately left outside the first single-authority implementation.
