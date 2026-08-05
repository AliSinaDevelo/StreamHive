# Release Checklist

Use this checklist for StreamHive releases. The current target is `v0.11.0`, the release with bounded repair continuations, multi-peer fairness evidence, dependency updates, and inventory scaling research.

## Preflight

```bash
listed=$(rg --files -g '*.go' | xargs gofmt -l); test -z "$listed"
go test ./...
go test -race ./...
go vet ./...
go test -bench=. -benchmem -run '^$' ./...
make test-fairness
go test ./replication -run '^TestResearchInventoryEnvelopeSizes$' -count=1 -v
go test . -run '^TestRun_retriesBlobPutAfterLostAck$' -count=1 -v
go test . -run '^TestRun_authenticatedRestartRepairsAndDeduplicatesContentBlob$' -count=1 -v
P2P_ADDR=127.0.0.1:17070 HEALTH_ADDR=127.0.0.1:18080 make demo-replication
make demo-compose
make demo-auth
make demo-repair
make demo-failure
make demo-continuation
go run . -version  # expected: 0.11.0
```

## Version

1. Update [internal/version/version.go](../internal/version/version.go) to the release semver.
2. Move completed [CHANGELOG.md](../CHANGELOG.md) entries from `[Unreleased]` into `[MAJOR.MINOR.PATCH] - YYYY-MM-DD`.
3. Commit the version bump:

```bash
git add internal/version/version.go CHANGELOG.md README.md docs/RELEASE.md
git commit -m "chore: release v0.11.0"
```

## Tag

```bash
git tag -a v0.11.0 -m "v0.11.0"
git push origin main
git push origin v0.11.0
```

## Release Notes

Highlight:

- `p2p.PeerSnapshots` and richer `/peers` metadata.
- Protocol reference for SHV1 frames, replication messages, limits, and repair behavior.
- TLS/mTLS identity guidance and explicit application-level auth gaps.
- Partial-sync resilience: unreadable or oversized requested blobs are skipped while later keys still send.
- `replication_blobs_skipped` metric.
- One-shot blob delivery waits for `blob.ack` and retries within the configured bounded budget.
- `replication_blob_ack_timeouts`, `replication_blob_retries`, and `replication_blob_acks_matched` metrics.
- Delivery outcome logs classify accepted, ACK-timeout, write-error, and canceled puts.
- A write failure closes the peer to prevent reuse after a potentially partial frame.
- Real TCP acceptance coverage drops the first ACK and verifies duplicate-safe retry completion.
- Exchanged bounded application identities in the shared-token handshake and exposed them through `TCPPeer`, `/peers`, and connection logs.
- Exact inbound identity allowlists via `PeerAuthAllowedIdentities` and `-peer-allow-ids`, with `peer_auth_identity_rejections` metrics.
- Authenticated Compose evidence for healthy identities, unlisted identity rejection, and tokenless demo compatibility.
- Aggregate anti-entropy counters for inventory advertisements, missing-key requests, and repair blob deliveries.
- Aggregate continuation counters for scheduled, completed, and dropped deferred repair work.
- Delivery logs distinguish `delivery=one-shot` from `delivery=anti-entropy` without high-cardinality metric labels.
- Authenticated restart/repair and duplicate-safe replay acceptance coverage.
- Repair demos that require a positive `replication_repair_blobs_sent` outcome.
- Bounded continuation demo evidence with periodic inventory disabled.
- Multi-peer continuation fairness acceptance under Go 1.22.x and 1.23.x.
- Anti-entropy inventory benchmark and the decision to retain the bounded flat `blob.has` protocol for v0.11.
- Dependency updates for checkout, setup-go, golangci-lint, testify, and upload-artifact with green CI evidence.

Attach or link the CI SBOM artifact when publishing GitHub release binaries.
