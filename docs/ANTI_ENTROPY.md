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

The checkpoint kept the flat wire format. A root-only digest cannot identify missing
keys, and a range-digest protocol would add divergence traversal, mixed-version fallback,
state, and new DoS limits. The follow-up implementation therefore tested aggregate
exchange backpressure before considering a new wire protocol.

## v0.12 Aggregate Exchange Budget

The end-to-end benchmark showed that frame bounds alone did not bound one complete
exchange. The implementation therefore adds scheduler state without changing the
replication messages:

- `-max-inventory-bytes` defaults to 16 MiB of encoded `blob.has` payload per peer
  exchange; `0` disables the byte cap.
- `-max-inventory-keys` defaults to 16,384 advertised keys per peer exchange; `0`
  disables the key cap.
- Each peer has one exclusive cursor. A budget hit sends a bounded chunk, records the
  last key, and schedules a delayed continuation. Concurrent startup and periodic
  triggers are coalesced, and disconnect or shutdown drops the cursor.
- Older peers still see ordinary `blob.has` frames. The receiver has no new state and
  remains bounded by each frame's `MaxKeys` and transport payload limit.
- A deliberately tiny byte cap still sends one minimum frame so an exchange can make
  progress rather than retrying the same key forever.

Run both measurements with:

```bash
make bench-inventory-wire
```

The following approximate Apple M1 samples use 65,536 keys and the default aggregate
budgets:

| Key bytes | Mode | Chunks | Frames | Wire bytes | Time | Allocations/op |
|---:|---|---:|---:|---:|---:|---:|
| 32 | Flat | 1 | 16 | 3.08 MB | 27.8 ms | 30.8 MB |
| 32 | Budgeted | 5 | 16 | 3.08 MB | 5.7 ms | 14.4 MB |
| 512 | Flat | 1 | 16 | 45.0 MB | 290 ms | 320 MB |
| 512 | Budgeted | 5 | 16 | 45.0 MB | 49 ms | 175 MB |

Five chunks means four data chunks plus a final empty cursor check. The total wire
volume and receiver-side probe count remain unchanged because the flat protocol is still
used, but the budgeted exchange limits the work held by each pager invocation and gives
other per-peer work a scheduling boundary. The aggregate counters
`replication_inventory_exchanges_started`, `replication_inventory_exchanges_completed`,
`replication_inventory_exchanges_limited`, `replication_inventory_exchanges_dropped`,
and `replication_inventory_exchanges_active` make that behavior observable without peer
or key labels.

This closes the v0.12 whole-exchange safety boundary without introducing digest or range
messages. Reconsider a versioned digest exchange only after a real workload still shows
unacceptable aggregate wire, CPU, probe, or continuation pressure under these budgets;
that work would need mixed-version fallback and independent DoS limits first.

## v0.12 Convergence Evidence

The budget is now exercised by a real-TCP restart acceptance path, not only unit tests and
benchmarks:

```bash
go test . -run '^TestRun_budgetedInventoryConvergesAfterTargetRestart$' -count=1 -v
make demo-inventory-budget
```

Both paths seed eight SHA-256 content-addressed blobs, disable periodic inventory, and cap each
startup exchange at one key and 128 encoded bytes. They observe a limited active source cursor,
disconnect the target before the cursor completes, verify the source gauge returns to zero, then
restart the durable target and require all eight keys, a completed exchange, and zero active
inventory work. The acceptance test also checks the limited, completed, keys-sent, active, and
dropped counters in JSON and the corresponding Prometheus text samples. This establishes the
current convergence and cleanup behavior before any digest or range protocol is considered.

The follow-up multi-peer mutation check is:

```bash
make test-inventory-fairness
```

It repeats a one-source/two-target real-TCP exchange under the same small budgets, mutates the
source between pages, disconnects one target while the other remains active, and requires both
targets to match the final content-addressed key set. The mutation removes only a key that has
not yet been advertised. That is deliberate: the current `blob.has`/`blob.missing`/`blob.put`
contract repairs additions but does not encode tombstones, so true delete propagation needs a
separate protocol design rather than an implicit anti-entropy promise.

The deletion decision is recorded in [DELETION_SEMANTICS.md](DELETION_SEMANTICS.md). For the
current add-only contract, a local delete is an eviction that a peer may repair; a future logical
delete must live in a separate versioned namespace with explicit retention and compaction rules.

## Research Sources

- [Dynamo: Amazon's highly available key-value store](https://www.amazon.science/publications/dynamo-amazons-highly-available-key-value-store)
  describes anti-entropy and Merkle trees, including the recalculation tradeoff as key
  ranges change.
- [HashiCorp memberlist state exchange](https://github.com/hashicorp/memberlist/blob/master/state.go)
  is a reference for periodic bounded push/pull synchronization.
- [Prometheus instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/)
  supports measuring queued/in-progress work and throughput without turning peer or blob
  identifiers into unbounded metric labels.
