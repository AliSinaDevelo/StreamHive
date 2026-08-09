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

The budgeted inventory restart acceptance test is:

```bash
go test . -run '^TestRun_budgetedInventoryConvergesAfterTargetRestart$' -count=1 -v
```

It seeds eight content-addressed blobs, forces one-key inventory continuations over real TCP,
disconnects the target while a source cursor is active, restarts the durable target, and checks
convergence plus JSON and Prometheus exchange counters with no active cursor left behind.

The multi-peer mutation and fairness acceptance target is:

```bash
make test-inventory-fairness
```

It runs five race-enabled repetitions with one source and two durable targets, mutates the source
between bounded pages, disconnects one target while the other converges, and checks exact final
key sets. The mutation deletes only a not-yet-advertised key because the current replication
protocol has no tombstone message.

The live-cursor mutation fallback acceptance target is:

```bash
make test-inventory-consistency
```

It runs three race-enabled repetitions, inserts a content-addressed key behind an active
one-key cursor, verifies the original exchange leaves it absent, and proves the next periodic
inventory pass repairs it. This documents the live-cursor contract rather than adding snapshot
state to the current protocol.

The local eviction and startup-only repair acceptance target is:

```bash
make test-eviction-repair
```

It runs five race-enabled repetitions, converges a durable target, stops it, deletes only the
target's local content-addressed blob, restarts with `-sync-interval 0s`, and verifies the exact
bytes return through startup inventory with no active exchange left behind. This is local
eviction rehydration, not a claim that `Delete` is a distributed logical revoke.

The real-TCP peer admission acceptance target is:

```bash
make test-peer-admission
```

It runs three race-enabled repetitions with a one-peer server cap and two clients, and checks
that the cap rejects exactly the excess connection while the admitted peer remains active.

The TLS and application-auth acceptance target is:

```bash
make test-tls-auth
```

It generates temporary certificates, proves CA and server-name verification before exact
application identity admission, replicates a content-addressed blob over the resulting connection,
and checks wrong-CA and wrong-hostname failures before peer registration.

The library mTLS acceptance target is:

```bash
make test-mtls
```

It runs three race-enabled repetitions with an ephemeral CA, server certificate, client certificate,
and unrelated client certificate. It proves a verified client exchanges a frame, while missing or
untrusted client certificates fail before the server calls `OnPeer` or its frame handler.

The CLI mTLS acceptance target is:

```bash
make test-mtls-cli
```

It runs three race-enabled repetitions proving strict inbound client verification, outbound client
certificate presentation, missing and untrusted client rejection before peer admission, and
fail-closed flag validation.

The restart-only TLS rotation acceptance target is:

```bash
make test-tls-rotation
```

It runs three race-enabled repetitions with a stable TCP listener, rotates to a new certificate
signed by the same CA, proves static-peer reconnect and certificate replacement, rejects malformed
startup material before readiness, restores the old material, and checks aggregate metrics.

The TLS credential-health acceptance target is:

```bash
make test-tls-credential-health
```

It runs three race-enabled repetitions covering aggregate server/client identity expiry,
warning suppression, expired and not-yet-valid startup rejection, Prometheus output, and
readiness behavior.

The health HTTP resource acceptance target is:

```bash
make test-health-server
```

It runs three race-enabled repetitions, rejects an oversized request header within the configured
limit, serves normal liveness traffic, and verifies context cancellation closes the health listener
across repeated starts.

The CLI shutdown acceptance target is:

```bash
make test-cli-shutdown
```

It runs three race-enabled repetitions covering non-positive grace validation, normal and
repeated context cancellation, pending one-shot ACK cancellation, forced transport deadline
expiry, reconnect cancellation, and repair-continuation shutdown behavior.

The bounded P2P drain acceptance target is:

```bash
make test-peer-drain
```

It runs three race-enabled repetitions covering cooperative handler shutdown,
pending authentication closure, deadline force-close, repeated drain calls, and
rejection of new admissions after the transport enters its draining state.

The lifecycle compaction and stale-peer snapshot acceptance targets are:

```bash
make test-lifecycle-compaction
make test-lifecycle-compose
```

The first runs the race-enabled real-TCP proof. The second builds the authenticated three-node
Compose topology, verifies present and tombstone convergence, compacts and restarts the source,
removes only the target lifecycle metadata, and checks checkpoint recovery plus raw SHA-256
retention. The Compose script bounds health polling and cleans up its project on success, failure,
or interruption.

The Prometheus exposition acceptance target is:

```bash
make test-prometheus-format
```

It runs three race-enabled repetitions, builds the complete transport/replication/TLS metric
snapshot, and parses the output to require safe names, one `HELP`/`TYPE` pair per sample,
deterministic ordering, and explicit counter/gauge classification.

The storage integrity status acceptance target is:

```bash
make test-storage-integrity
```

It runs three race-enabled repetitions covering bounded native paging, legacy-lister fallback,
content-addressed verification, opaque-key accounting, missing and corrupt entries, cancellation,
raw-only health output, generic failure responses, and JSON/Prometheus metric accounting over a
real durable store.

The offline durable-store verification acceptance target is:

```bash
make test-storage-verify
```

It runs three race-enabled repetitions covering the healthy aggregate JSON contract, required
`-store-dir` validation, flag exclusivity, corruption failure status, and the guarantee that a
failed verification does not mutate the durable bytes. The command is offline and does not open a
listener.

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
- `make test-lifecycle-compose` to verify authenticated three-node lifecycle compaction, source restart, and stale-peer checkpoint recovery
- `make demo-auth` to verify authenticated peer identities, allowlist and token rejection before blob storage, and restart repair in one acceptance path
- `make demo-repair` to overwrite durable content, observe `replication_corrupt_blobs_detected`, and verify a positive `replication_repair_blobs_sent` outcome plus recovered SHA-256 bytes
- `make demo-failure` to verify peer reconnect plus repair after a node restart
- `make demo-continuation` to force an 8-byte repair budget and verify deferred delivery through continuation counters without periodic inventory
- `make demo-inventory-budget` to force one-key/128-byte startup inventory continuations, interrupt a real TCP target, restart its durable store, and verify complete convergence plus JSON/Prometheus counters
- `make test-inventory-fairness` to repeat the multi-peer mutation/reconnect convergence proof under the race detector
- `make test-eviction-repair` to repeat startup-only rehydration after local durable eviction under the race detector
- `make test-inventory-consistency` to repeat periodic repair after a behind-cursor mutation under the race detector
- `make test-inventory-status` to repeat live inventory fingerprint mismatch/equality after bounded real-TCP repair under the race detector
- `make test-inventory-status-metrics` to repeat successful and failed inventory-status accounting, including JSON/Prometheus samples, under the race detector
- `make test-storage-verify` to repeat offline aggregate durable-store verification, including non-zero corruption handling, under the race detector
- `make test-peer-admission` to repeat real-TCP peer-cap rejection and active-peer observability under the race detector
- `make test-tls-auth` to repeat TLS certificate verification, application identity admission, and pre-registration certificate failures under the race detector
- `make test-mtls` to repeat library mTLS admission, bounded TLS failure handling, and frame exchange under the race detector
- `make test-mtls-cli` to repeat CLI mTLS certificate admission, pre-registration rejection, and fail-closed flag validation under the race detector
- `make test-tls-rotation` to repeat restart-only certificate rotation, bounded static-peer reconnect, malformed startup rejection, and rollback under the race detector
- `make test-tls-credential-health` to repeat aggregate TLS identity validity, expiry warning, startup rejection, and readiness checks under the race detector
- `make test-prometheus-format` to repeat typed, sorted, label-free `HELP`/`TYPE` health exposition checks under the race detector
- `make test-health-server` to repeat bounded health HTTP request and graceful-shutdown checks under the race detector
- `make test-peer-drain` to repeat bounded P2P drain, admission, and forced-close checks under the race detector
- Coverage profile upload as a workflow artifact (`coverage-<go-version>.out`)
- **SBOM** job: CycloneDX JSON via `cyclonedx-gomod`, uploaded as `sbom-cyclonedx`

Workflow steps use **pinned action SHAs** (immutable) instead of floating version tags.

Dependabot opens weekly update PRs for Go modules and GitHub Actions.

## Releases

Tag `v*` versions aligned with [CHANGELOG.md](../CHANGELOG.md) and [internal/version/version.go](../internal/version/version.go). Attach the CycloneDX artifact from CI when publishing release binaries. See [GOVERNANCE.md](GOVERNANCE.md) for branch protection and signing guidance.
