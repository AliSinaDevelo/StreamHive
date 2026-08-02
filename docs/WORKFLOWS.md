# Workflows

## Local development

1. **Format and static checks** (optional): `make lint` requires `golangci-lint` on `PATH`.
2. **Fast feedback**: `make test`
3. **Concurrency**: `make test-race`
4. **Coverage**: `make cover` writes `coverage.out` and prints `go tool cover -func` output.

## Benchmarks

Run local microbenchmarks for framing and the in-memory blob store:

```bash
go test -bench=. -benchmem -run '^$' ./...
```

The current benchmark coverage focuses on `SHV1` frame round-trips and `MemoryStore` `Put`/`Get` throughput. Treat results as local-machine signals, not portable service-level guarantees.

## Continuous integration

GitHub Actions (`.github/workflows/ci.yml`) runs on pushes and pull requests to `main`:

- `go vet ./...`
- `go test -race -count=1 ./...` on Go 1.22.x and 1.23.x
- `golangci-lint` with `.golangci.yml`
- `govulncheck ./...` on a current patched Go toolchain (separate from the compatibility matrix)
- `make demo-replication` with fixed localhost ports
- `make demo-compose` to verify 3-node durable rehydration
- `make demo-auth` to verify shared-token admission and rejection before blob storage
- `make demo-repair` to verify periodic repair after local durable corruption
- `make demo-failure` to verify peer reconnect plus repair after a node restart
- Coverage profile upload as a workflow artifact (`coverage-<go-version>.out`)
- **SBOM** job: CycloneDX JSON via `cyclonedx-gomod`, uploaded as `sbom-cyclonedx`

Workflow steps use **pinned action SHAs** (immutable) instead of floating version tags.

Dependabot opens weekly update PRs for Go modules and GitHub Actions.

## Releases

Tag `v*` versions aligned with [CHANGELOG.md](../CHANGELOG.md) and [internal/version/version.go](../internal/version/version.go). Attach the CycloneDX artifact from CI when publishing release binaries. See [GOVERNANCE.md](GOVERNANCE.md) for branch protection and signing guidance.
