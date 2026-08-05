# Anti-Entropy Inventory Research

## Question

Should StreamHive replace its bounded `blob.has` key list with a compact digest or a
range-level Merkle exchange as stores grow?

## Method

The design-only helpers in
[`replication/anti_entropy_research_test.go`](../replication/anti_entropy_research_test.go)
compare three envelopes over sorted 32-byte keys:

- **Flat JSON**: the current `blob.has` message, bounded by `MaxKeys` and the transport
  frame limit.
- **Rolling root**: one SHA-256 digest over the ordered key stream. This is a lower bound
  for a digest protocol because it can detect equality but cannot identify missing keys.
- **Range digests**: one Merkle root for each 256-key range. This estimates a bounded
  range-comparison exchange but does not model the follow-up messages needed to descend
  into a divergent range.

Run the measurement with:

```bash
go test ./replication -run '^TestResearchInventoryEnvelopeSizes$' -count=1 -v
go test ./replication -run '^$' -bench '^BenchmarkResearchInventory$' -benchmem -benchtime=200ms
```

The numbers below are a reference run on an Apple M1 with Go's local benchmark toolchain;
they are directional, not service-level guarantees.

## Reference Results

| Key bytes | Keys | Flat wire | Flat time | Flat allocs | Root wire | Root time | Range wire | Range time |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 32 | 128 | 6,044 B | 15.1 us | 131 | 112 B | 4.0 us | 152 B | 22.4 us |
| 32 | 1,024 | 48,156 B | 55.8 us | 1,027 | 113 B | 24.8 us | 448 B | 165.5 us |
| 32 | 4,096 | 192,540 B | 229.4 us | 4,101 | 113 B | 86.1 us | 1,648 B | 706.0 us |
| 512 | 128 | 87,964 B | 65.5 us | 131 | 112 B | 43.1 us | 152 B | 56.0 us |
| 512 | 1,024 | 703,516 B | 494.8 us | 1,034 | 113 B | 244.4 us | 448 B | 399.8 us |
| 512 | 4,096 | 2,813,980 B | 1.89 ms | 4,109 | 113 B | 947.4 us | 1,648 B | 1.76 ms |

At 4,096 keys, the range-digest model allocated about 528 KB/op versus 478 KB/op for
32-byte flat JSON because it builds 16 independent trees. A root-only digest allocated
about 352 B/op, but it cannot produce a missing-key set on its own. The maximum 512-byte
key case produced a 2.81 MB flat payload, still below the default 4 MiB transport frame,
but with materially less headroom for larger limits or additional envelope fields.

## Decision for v0.11

Keep the current flat inventory protocol. It is simple, version-compatible, already split
at both the key and frame bounds, and remains modest for canonical content-addressed keys
at the current `MaxKeys` limit. A digest root is an incomplete repair protocol; a range
scheme saves wire bytes only by adding comparison state, follow-up traversal, versioning,
and more CPU/memory work. That trade is not justified by this measurement.

Revisit the decision when a real workload shows inventories regularly approaching the
transport frame limit, stores materially exceed the current key bound, or inventory CPU
becomes a measurable part of the node budget. Any follow-up should specify mixed-version
fallback, range boundaries, digest domain separation, missing-key retrieval, and its own
DoS limits before changing the wire protocol.

## v0.12 Wire Pressure Checkpoint

After the native store indexes landed, the end-to-end research benchmark measured the
current flat exchange through `sendBlobHas`, JSON decode, and receiver-side
`BlobStore.Has` probes. Run it with:

```bash
make bench-inventory-wire
```

The local Apple M1 samples below use `MaxKeys=4096` and the default 4 MiB frame limit:

| Key bytes | Keys | Frames/op | Wire bytes/op | Exchange time | Allocations/op | Probes/op |
|---:|---:|---:|---:|---:|---:|---:|
| 32 | 4,096 | 1 | 192,540 B | 1.84 ms | 2.29 MB | 4,096 |
| 32 | 65,536 | 16 | 3,080,640 B | 27.2 ms | 30.8 MB | 65,536 |
| 64 | 4,096 | 1 | 372,764 B | 3.06 ms | 4.26 MB | 4,096 |
| 64 | 65,536 | 16 | 5,964,224 B | 45.6 ms | 53.5 MB | 65,536 |
| 512 | 4,096 | 1 | 2,813,980 B | 17.0 ms | 25.6 MB | 4,096 |
| 512 | 65,536 | 16 | 45,023,680 B | 293 ms | 320 MB | 65,536 |

The canonical 32-byte case remains below the per-frame limit and is a reasonable
bounded exchange, but total work is visible at 65,536 keys. The maximum 512-byte key
case also keeps each frame below 4 MiB while producing about 45 MB of total wire data,
293 ms of exchange time, 320 MB of allocations, and one receiver probe per key. The
current protocol is therefore still compatible and frame-bounded, but it does not yet
have an aggregate whole-exchange byte or probe budget.

The checkpoint keeps the flat wire format. A root-only digest cannot identify missing
keys, and a range-digest protocol would add divergence traversal, mixed-version fallback,
state, and new DoS limits. The next task is to decide whether aggregate exchange
backpressure or a configurable inventory budget is enough before revisiting a new wire
protocol.

## Research Sources

- [Dynamo: Amazon's highly available key-value store](https://www.amazon.science/publications/dynamo-amazons-highly-available-key-value-store)
  describes anti-entropy and Merkle trees, including the recalculation tradeoff as key
  ranges change.
- [HashiCorp memberlist state exchange](https://github.com/hashicorp/memberlist/blob/master/state.go)
  is a reference for periodic bounded push/pull synchronization.
- [Prometheus instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/)
  supports measuring queued/in-progress work and throughput without turning peer or blob
  identifiers into unbounded metric labels.
