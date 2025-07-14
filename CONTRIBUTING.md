# Contributing

## Requirements

- Go 1.22+
- Optional: `golangci-lint` for `make lint`

## Before opening a PR

```bash
make vet
make test-race
make lint   # if golangci-lint is installed
```

## Project layout

- `p2p/` — transport, framing, metrics
- `storage/` — blob store interfaces
- `internal/version` — semver constant
- `docs/` — design, deployment, governance
- `main.go` — CLI and health HTTP server

Keep changes focused; prefer small commits and tests that fail before the fix lands.

## Repository settings

Recommended GitHub settings are summarized in [docs/GOVERNANCE.md](docs/GOVERNANCE.md) (branch protection, required checks).
