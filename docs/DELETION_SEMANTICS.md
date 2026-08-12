# Deletion And Tombstone Semantics

Status: v0.12.0 raw-blob contract, tracked by issue #33. The v0.13 lifecycle follow-up is
defined in [LIFECYCLE_V0_13.md](LIFECYCLE_V0_13.md), tracked by issue #49.

## Decision

StreamHive keeps blob replication add-only for the current contract.

- A content-addressed blob is immutable: its SHA-256 key names its bytes.
- BlobStore.Delete is local eviction or garbage collection, not a replicated logical delete.
- FileStore.Delete removes only regular blob files; missing keys remain a no-op, while non-regular
  keyed paths are rejected with `storage.ErrNonRegularEntry`.
- A local delete may be repaired from a connected peer during startup or periodic anti-entropy.
- The current blob.has, blob.missing, blob.get, and blob.put messages do not carry tombstones,
  versions, or causal context.
- No tombstone or delete message is added in v0.12.0.

This is deliberate. A blob store can safely answer "is this byte object present here?" without
pretending that removing one replica revokes the object everywhere. Logical deletion needs a
separate namespace and lifecycle contract.

## Why The Boundary Matters

Removing a file without retaining deletion state is unsafe for a replicated object. A disconnected
peer can reconnect with the old blob and an add-only anti-entropy pass will treat that blob as
missing elsewhere and restore it. A deletion record must therefore outlive the data it suppresses
until every peer that could reintroduce the data has observed the record, or until an explicit
retention protocol makes that guarantee.

This is the same broad failure mode documented by distributed stores that use tombstones:
deletion is represented as state that participates in repair, not as an unobserved physical
absence. Apache Cassandra retains deletion markers through a grace period to prevent a down
replica from resurrecting old data during repair. etcd records a key tombstone in its MVCC
history and later removes old generations through revision compaction.

## Options Considered

| Model | Strength | Cost or risk | StreamHive decision |
|---|---|---|---|
| Local eviction only | Fits immutable blobs and the current wire; no delete conflicts | A peer may rehydrate a locally evicted blob | Keep now; document as cache/object-store semantics |
| Single-authority tombstone | Simple ordering with an epoch and sequence | Requires authority identity, failover, durable sequencing, and admission rules | Future namespace design |
| Multi-writer version set | Keeps writes available across partitions and can represent concurrent delete/put conflicts | Version metadata, conflict policy, bounded context, and tombstone retention become part of every repair | Defer; study only with a concrete workload |
| TTL expiration | Useful for cache-like data and can share tombstone machinery | Clock/expiry semantics, grace windows, and compaction pressure still need definition | Defer until a TTL use case exists |

The Dynamo paper is a useful reference for the multi-writer cost: vector clocks provide causal
version context while anti-entropy repairs divergent replicas, but both the metadata and conflict
policy become part of the storage contract. StreamHive should not import that machinery merely to
make BlobStore.Delete look distributed.

## Future Namespace Shape

If an application needs logical deletion, model it separately from the immutable blob namespace:

    logical key -> { state, blob reference, version token, writer identity }

The future record would need:

1. A durable version token that totally orders updates from one authority or detects concurrent
   updates from multiple writers.
2. A delete state that is advertised and repaired like a put.
3. A read rule for concurrent put/delete versions.
4. Retention and compaction rules that prevent a stale peer from resurrecting data after a
   tombstone is discarded.
5. A garbage-collection rule that removes an unreferenced immutable blob only after the liveness
   record no longer needs it.

This record intentionally does not choose a wire encoding. A future implementation issue must
first choose authority versus multi-writer semantics, then specify restart and mixed-version
behavior before adding a message.

The v0.13 research decision now selects an operator-fenced single authority per logical
namespace, with durable `(epoch, sequence)` lifecycle tokens and per-peer repair watermarks.
It still does not change this v0.12 raw-blob contract or add a tombstone message. See
[LIFECYCLE_V0_13.md](LIFECYCLE_V0_13.md) for the ordering, retention, compaction, mixed-version,
and implementation boundaries.

## Operational Budgets

Any future tombstone design must bound:

- tombstone bytes and entries retained per namespace;
- maximum tombstones in one advertisement or repair response;
- version metadata bytes per logical key;
- compaction and garbage-collection concurrency;
- replay age for disconnected peers;
- aggregate counters and gauges without peer, blob, or logical-key labels.

The existing inventory byte/key budgets remain the relevant model. A delete path must not bypass
them because deletion metadata is smaller than blob data.

## Model And Acceptance Plan

Before implementation, model these transitions:

- local eviction while a peer remains connected;
- put, delete, reconnect, and restart in every order;
- a stale peer returning after the retention window;
- concurrent put and delete from two writers;
- tombstone compaction while a repair cursor is active;
- an older peer that understands only the current add-only messages.

The current acceptance contract is intentionally narrower and is covered by
TestRun_budgetedInventoryConvergesAcrossPeersAndSourceMutation: deleting a source key before
it is advertised is valid, while deleting an already-advertised key is not claimed to propagate.
The dedicated `make test-eviction-repair` acceptance target separately proves that deleting an
already-replicated blob from one stopped local store is rehydrated by startup anti-entropy when a
peer still owns the immutable content. It does not add or imply a logical delete operation.

## Mixed-Version Contract

Older and newer StreamHive nodes continue to exchange ordinary blob.has, blob.missing, blob.get,
and blob.put messages. No node is expected to interpret a delete that the current protocol cannot
encode. This keeps the v0.12 inventory budget and cursor changes wire-compatible while the
lifecycle design remains open.

## Research Sources

- [Dynamo: Amazon's highly available key-value store](https://www.amazon.science/publications/dynamo-amazons-highly-available-key-value-store)
  is the primary design reference for vector-clock version context and anti-entropy tradeoffs.
- [Apache Cassandra compaction and tombstones](https://cassandra.apache.org/doc/stable/cassandra/managing/operating/compaction/overview.html)
  documents deletion markers, grace periods, repair, and zombie prevention.
- [etcd data model](https://etcd.io/docs/v3.7/learning/data_model/)
  documents MVCC revisions, key tombstones, generations, and compaction.
- [RocksDB deletion-triggered compaction](https://rocksdb.org/blog/)
  illustrates why an embedded store's delete marker and physical space reclamation are separate
  concerns.
