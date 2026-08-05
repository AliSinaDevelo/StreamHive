# Ordered Inventory Decision

This note records the v0.12 follow-up to the bounded inventory pager. The wire
protocol remains unchanged; this is a storage-side performance decision.

## Decision

`MemoryStore` uses a generic B-tree index for known keys. The data map remains the
source of truth, while the index provides ordered insertion, deletion, and cursor
pages without turning each write into a linear slice shift. `ListKeyPage` now seeks to
the exclusive cursor and visits only the requested page.

`FileStore` keeps the existing bounded directory scan for now. Go's directory API gives
bounded chunks and preserves progress on one open handle, but directory order is not a
bytewise key order and the API has no seek-by-name operation. A durable manifest or
index would need crash recovery, rebuild, mutation, and multi-process ownership rules;
that is a separate design rather than an implicit file format change.

## Measurements

The benchmark compares a complete inventory with the native page path. Values vary by
machine; these are local Apple M1 samples after the MemoryStore index change:

| Inventory | Full `ListKeys` | Indexed pages | Interpretation |
| --- | ---: | ---: | --- |
| 4,096 keys | 0.09 ms, 131 KB | 0.17 ms, 144 KB | Paging adds small per-page overhead while retaining bounded page state. |
| 65,536 keys | 2.22 ms, 2.54 MB | 2.40 ms, 2.65 MB | The repeated full-scan curve disappears for MemoryStore. |

Before the index, the 65,536-key paged path measured about 824.5 ms and 9.4 MB of
cumulative allocations. The comparison is intentionally about scaling, not a portable
latency promise. Run it with:

```bash
make bench-inventory
```

## Rejected Alternatives

- A sorted `[]string` index would make page seeks cheap, but insertion and deletion
  would shift O(N) elements. The B-tree keeps both updates and seeks logarithmic.
- A callback or one-pass iterator alone does not solve FileStore ordering. Directory
  entries can be streamed in bounded chunks, but emitting deterministic bytewise pages
  requires either retaining a full candidate set or maintaining an ordered index.
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
  the convergence mechanism rather than assuming one snapshot.

References: [Go `File.ReadDir`](https://pkg.go.dev/os#File.ReadDir), [Pebble
iterators](https://github.com/cockroachdb/pebble/blob/master/iterator.go), [Badger
iterators](https://pkg.go.dev/github.com/dgraph-io/badger/v4#Iterator), and the
[Google generic B-tree](https://pkg.go.dev/github.com/google/btree).
