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

- `p2p/` — networking primitives
- `docs/` — design and operational notes
- `main.go` — CLI entrypoint for trying the transport

Keep changes focused; prefer small commits and tests that fail before the fix lands.
