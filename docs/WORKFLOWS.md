# Workflows

## Local development

1. **Format and static checks** (optional): `make lint` requires `golangci-lint` on `PATH`.
2. **Fast feedback**: `make test`
3. **Concurrency**: `make test-race`
4. **Coverage**: `make cover` writes `coverage.out` and prints `go tool cover -func` output.

The focused delivery acceptance test is:

```bash
go test . -run '^TestRun_retriesBlobPutAfterLostAck$' -count=1 -v
```

It uses a real TCP transport, withholds the first ACK, and verifies that the second
idempotent put is acknowledged and exits cleanly.

The authenticated restart and duplicate-safety acceptance test is:

```bash
go test . -run '^TestRun_authenticatedRestartRepairsAndDeduplicatesContentBlob$' -count=1 -v
```

It authenticates bounded peer identities, repairs a deleted content-addressed blob after
the target process restarts, repairs a deliberately corrupted content-addressed file,
and verifies that an exact replay is acknowledged as a duplicate.

## Benchmarks

Run local microbenchmarks for framing and the in-memory blob store:

```bash
go test -bench=. -benchmem -run '^$' ./...
```

The current benchmark coverage focuses on `SHV1` frame round-trips and `MemoryStore` `Put`/`Get` throughput. Treat results as local-machine signals, not portable service-level guarantees.

Compare full inventory materialization with the bounded native pager at 4,096 and
65,536 keys:

```bash
make bench-inventory
```

The benchmark reports cumulative allocations from repeated page calls and separates the
one-time FileStore index build. MemoryStore and FileStore page through ordered B-trees;
FileStore rebuilds from `File.ReadDir` chunks when its directory modification stamp
changes. See [INVENTORY_ITERATORS.md](INVENTORY_ITERATORS.md) for the measurements,
startup budget, and rejected alternatives.

The resource-budget acceptance check is separate from throughput benchmarks:

```bash
make test-budgets
```

The anti-entropy inventory research benchmark is:

```bash
go test ./replication -run '^TestResearchInventoryEnvelopeSizes$' -count=1 -v
go test ./replication -run '^$' -bench '^BenchmarkResearchInventory$' -benchmem -benchtime=200ms
```

It compares the current bounded JSON inventory with design-only digest envelopes; see
[ANTI_ENTROPY.md](ANTI_ENTROPY.md) for the v0.11 decision.

Measure the complete indexed-store inventory exchange, including frame count, encoded
bytes, receiver key probes, allocations, and the default budgeted continuation path:

```bash
make bench-inventory-wire
```

This is a research checkpoint, not a protocol compatibility test. It intentionally
keeps the current flat `blob.has` wire format while comparing one unbroken exchange with
the default 16 MiB / 16,384-key per-peer budget. To exercise the same limits manually,
run the CLI with `-max-inventory-bytes 16777216 -max-inventory-keys 16384`.

## Continuous integration

GitHub Actions (`.github/workflows/ci.yml`) runs on pushes and pull requests to `main`:

- `go vet ./...`
- `go test -race -count=1 ./...` on Go 1.22.x and 1.23.x
- `make test-fairness` in a dedicated Go 1.22.x / 1.23.x matrix job to prove a blocked repair peer cannot serialize a healthy peer
- `make test-budgets` in a dedicated Go 1.23.x job to prove the configured per-peer continuation queue saturates at `MaxKeys` and drops excess work
- `make bench-inventory` in a dedicated Go 1.23.x job to keep the paged inventory comparison measurable
- `make bench-inventory-wire` in a dedicated Go 1.23.x job to keep flat and aggregate-budgeted end-to-end inventory wire pressure measurable
- Real TCP lost-ACK acceptance coverage through the main test package
- `golangci-lint` with `.golangci.yml`
- `govulncheck ./...` on a current patched Go toolchain (separate from the compatibility matrix)
- `make demo-replication` with fixed localhost ports
- `make demo-compose` to verify 3-node durable rehydration
- `make demo-auth` to verify authenticated peer identities, allowlist and token rejection before blob storage, and restart repair in one acceptance path
- `make demo-repair` to overwrite durable content, observe `replication_corrupt_blobs_detected`, and verify a positive `replication_repair_blobs_sent` outcome plus recovered SHA-256 bytes
- `make demo-failure` to verify peer reconnect plus repair after a node restart
- `make demo-continuation` to force an 8-byte repair budget and verify deferred delivery through continuation counters without periodic inventory
- Coverage profile upload as a workflow artifact (`coverage-<go-version>.out`)
- **SBOM** job: CycloneDX JSON via `cyclonedx-gomod`, uploaded as `sbom-cyclonedx`

Workflow steps use **pinned action SHAs** (immutable) instead of floating version tags.

Dependabot opens weekly update PRs for Go modules and GitHub Actions.

## Releases

Tag `v*` versions aligned with [CHANGELOG.md](../CHANGELOG.md) and [internal/version/version.go](../internal/version/version.go). Attach the CycloneDX artifact from CI when publishing release binaries. See [GOVERNANCE.md](GOVERNANCE.md) for branch protection and signing guidance.
