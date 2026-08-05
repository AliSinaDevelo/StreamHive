# Ordered Inventory Decision

This note records the v0.12 follow-up to the bounded inventory pager. The wire
protocol remains unchanged; this is a storage-side performance decision.

## Decision

Both stores use a process-local generic B-tree index for known keys. MemoryStore keeps
the data map as the source of truth, while the B-tree provides ordered insertion,
deletion, and cursor pages without turning each write into a linear slice shift.

FileStore builds its index lazily from the existing hex-encoded blob filenames using
bounded `File.ReadDir` chunks. The durable files remain authoritative. A directory
modification timestamp invalidates the process-local index, so a second FileStore
handle adding or deleting a blob is picked up on the next inventory call; successful
mutations through the current handle update the index directly. No sidecar file or
durable format change is introduced.

## Measurements

The benchmark compares complete inventory materialization, indexed cursor pages, and
the one-time FileStore index build. Values vary by machine; these are local Apple M1
samples from `make bench-inventory` plus the focused one-iteration index-build
measurement:

| Store / size | Full `ListKeys` | Indexed pages | First index build |
| --- | ---: | ---: | ---: |
| MemoryStore / 4,096 | 0.095 ms, 131 KB | 0.092 ms, 144 KB | maintained during writes |
| MemoryStore / 65,536 | 1.54 ms, 2.54 MB | 1.80 ms, 2.65 MB | maintained during writes |
| FileStore / 4,096 | 0.089 ms, 164 KB | 0.133 ms, 182 KB | 3.20 ms, 0.85 MB |
| FileStore / 65,536 | 1.76 ms, 2.62 MB | 7.57 ms, 2.82 MB | 75.1 ms, 13.3 MB |

Before the MemoryStore index, the 65,536-key repeated-scan page path measured about
824.5 ms and 9.4 MB of cumulative allocations. The comparison is intentionally about
scaling and budget visibility, not a portable latency promise. FileStore pays an
explicit O(N) lazy rebuild in exchange for indexed steady-state cursors and keeps its
simple durable file format.

```bash
make bench-inventory
```

## Rejected Alternatives

- A sorted `[]string` index would make page seeks cheap, but insertion and deletion
  would shift O(N) elements. The B-tree keeps both updates and seeks logarithmic.
- A callback or one-pass iterator alone does not solve cursor calls across independent
  page requests. Directory entries can be streamed in bounded chunks, but emitting
  deterministic bytewise pages requires either retaining a full candidate set or
  maintaining an ordered index.
- A durable manifest would reduce rebuild cost but introduces crash ordering, recovery,
  rebuild, and multi-process ownership rules. It remains deferred until a workload
  justifies a new durable format.
- Replacing FileStore with Pebble or Badger would provide strong ordered iterators, but
  it would change the deliberately small durable file format and its operational
  failure surface. That option remains a future storage backend, not a hidden
  dependency of the current FileStore.

## Contract

The public `storage.BlobKeyPager` contract remains:

- keys are returned in bytewise order and are strictly greater than `after`;
- a page contains at most `limit` keys and `next` is its last key;
- an empty page means enumeration is complete;
- context cancellation is checked while enumerating;
- mutations between calls may be reflected, so callers retain periodic inventory as
  the convergence mechanism rather than assuming one snapshot; FileStore also
  refreshes its process-local index when the directory modification stamp changes.

References: [Go `File.ReadDir`](https://pkg.go.dev/os#File.ReadDir), [Pebble
iterators](https://github.com/cockroachdb/pebble/blob/master/iterator.go), [Badger
iterators](https://pkg.go.dev/github.com/dgraph-io/badger/v4#Iterator), and the
[Google generic B-tree](https://pkg.go.dev/github.com/google/btree).
