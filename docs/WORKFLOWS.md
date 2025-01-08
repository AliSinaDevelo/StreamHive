# Workflows

## Local development

1. **Format and static checks** (optional): `make lint` requires `golangci-lint` on `PATH`.
2. **Fast feedback**: `make test`
3. **Concurrency**: `make test-race`
4. **Coverage**: `make cover` writes `coverage.out` and prints `go tool cover -func` output.

## Continuous integration

GitHub Actions (`.github/workflows/ci.yml`) runs on pushes and pull requests to `main`:

- `go vet ./...`
- `go test -race -count=1 ./...` on Go 1.22.x and 1.23.x
- `golangci-lint` with `.golangci.yml`
- `govulncheck ./...`

Dependabot opens weekly update PRs for Go modules and GitHub Actions.

## Releases

There is no automated release pipeline yet. Tagging and binaries can be added when the API stabilizes.
