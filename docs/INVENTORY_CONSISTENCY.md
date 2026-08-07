# Inventory Consistency

Status: v0.12.0 raw-blob inventory contract, tracked by issue #35. Lifecycle replication is a
separate v0.13 journal design in [LIFECYCLE_V0_13.md](LIFECYCLE_V0_13.md), tracked by issue #49.

## Decision

StreamHive's native inventory pager and per-peer exchange cursor are live views, not
snapshot or revision-based scans.

- `BlobKeyPager` returns a bounded bytewise page strictly after an exclusive cursor.
- A continuation resumes from the last key sent; it does not retain a historical store view.
- A key inserted after the cursor and at or before its position may wait for the next periodic
  or reconnect inventory pass.
- `-sync-interval 0s` means startup-only inventory. It does not promise snapshot-consistent
  convergence when the source mutates during an exchange.
- A key inserted after the cursor and after its position can be picked up by the current live
  pass, subject to the same bounded scheduling and repair budgets.
- Deletion remains governed by [DELETION_SEMANTICS.md](DELETION_SEMANTICS.md); the add-only
  wire contract does not turn absence into a replicated delete.

This keeps the current pager, aggregate byte/key budgets, and mixed-version `blob.has` wire
messages small. It also makes the required operational setting explicit: a long-lived mutable
store needs a finite periodic inventory interval or an equivalent reconnect path.

## Mutation Matrix

| Mutation relative to the cursor | Current exchange | Recovery path |
| --- | --- | --- |
| Existing key already before the cursor | Already advertised or out of scope for this pass | Next periodic/reconnect pass for a missing replica |
| New key greater than the cursor | Eligible for a later page in the same exchange | Current continuation, then periodic fallback |
| New key at or before the cursor | Not eligible for the current continuation | Next periodic/reconnect pass |
| Source local eviction | Source may no longer send the blob | Local deletion semantics and a separate logical-liveness design |
| Target disconnect during continuation | Cursor entry is discarded | Reconnect or a fresh periodic/startup exchange |

The matrix describes eventual repair behavior, not a transactional snapshot. Inventory pages can
observe different store states between calls, and the receiver may see a valid prefix before the
source finishes its exchange.

## Why This Boundary

There are three broad ways to make a changing ordered store resumable:

1. Keep a live cursor and run complete or periodic repair passes. This is the current choice;
   it keeps memory and protocol state bounded and lets a reconnect discard stale cursor state.
2. Pin a snapshot or revision for the entire exchange. This gives a stable view, but requires
   snapshot lifetime, retention, cancellation, and storage/version accounting under slow peers.
3. Track mutations by generation or revision and restart or replay the affected ranges. This
   avoids a full snapshot but makes mutation metadata, replay retention, and mixed-version
   behavior part of the storage contract.

HashiCorp memberlist documents complete push/pull state synchronization as an explicitly periodic,
expensive operation, with a zero interval disabling it. Cassandra documents periodic anti-entropy
repair over full or sub-ranges because best-effort propagation is not enough to guarantee
convergence. etcd demonstrates the stronger revision/history alternative: revisions support
resumable watches and point-in-time reads, while history retention and compaction become explicit
correctness and resource concerns.

StreamHive does not currently have a workload that justifies snapshot retention, mutation logs, or
a new revision-bearing message. The live cursor plus periodic fallback is the smallest honest
contract for v0.12.

## Acceptance Evidence

Run the focused proof with:

```bash
make test-inventory-consistency
```

`TestRun_periodicInventoryRepairsKeyAddedBehindLiveCursor` uses four initial SHA-256-addressed
blobs and one-key/128-byte inventory budgets. It waits until the first source cursor is active,
adds a new content-addressed key that sorts before the first cursor key, waits for the original
exchange to complete, and verifies the target still has only the initial set. It then observes the
next periodic exchange repair the late key and checks that the source has no active inventory work.

The test is deliberately not a snapshot test. Its purpose is to prevent a future change from
silently claiming stronger semantics than the pager and wire protocol provide.

## Future Threshold

Reconsider a generation, snapshot, or revision API only when a real workload demonstrates one of
these conditions:

- periodic repair latency is outside the application's recovery objective;
- mutation rates cause repeated full inventory work that exceeds the configured byte/key budgets;
- operators need a bounded catch-up window while periodic inventory is disabled;
- a logical namespace needs ordered updates or deletes rather than immutable blob availability.

Any follow-up must specify retention, cancellation, restart, compaction, mixed-version fallback,
and frame/storage/metric budgets before changing the wire contract.

The v0.13 lifecycle design does not strengthen this raw-blob cursor. Logical records use a
durable ordered mutation journal and per-peer watermarks, with a bounded snapshot fallback when
a peer has fallen behind the retained journal floor. This prevents a live raw inventory cursor
from being mistaken for a deletion-safe revision stream.

## Research Sources

- [HashiCorp memberlist state exchange](https://github.com/hashicorp/memberlist/blob/master/state.go)
  implements periodic push/pull triggers and complete state exchange.
- [HashiCorp memberlist configuration](https://github.com/hashicorp/memberlist/blob/master/config.go)
  documents the cost and zero-disable behavior of `PushPullInterval`.
- [Apache Cassandra Dynamo architecture](https://cassandra.apache.org/doc/3.11/cassandra/architecture/dynamo.html)
  describes periodic anti-entropy repair and full/sub-range Merkle comparisons.
- [etcd API guarantees](https://etcd.io/docs/v3.7/learning/api_guarantees/)
  documents revision-ordered, resumable watch history and its bounded availability window.
- [etcd data model](https://etcd.io/docs/v3.7/learning/data_model/)
  documents revisioned B-tree state, historical versions, tombstones, and compaction.
