# Release Checklist

Use this checklist for StreamHive releases. The current target is `v0.13.0`, the release that adds the opt-in, authenticated lifecycle journal, bounded repair sessions, operator-fenced compaction, and stale-peer snapshot recovery while preserving raw-blob compatibility.

## Preflight

```bash
listed=$(rg --files -g '*.go' | xargs gofmt -l); test -z "$listed"
go test ./...
go test -race -count=1 ./...
go vet ./...
go test -bench=. -benchmem -run '^$' ./...
make test-fairness
make test-peer-drain
make test-cli-shutdown
make test-health-server
make test-prometheus-format
make test-tls-auth
make test-mtls
make test-mtls-cli
make test-tls-rotation
make test-tls-credential-health
make test-lifecycle-compaction
make test-lifecycle-compose
go test ./replication -run '^TestResearchInventoryEnvelopeSizes$' -count=1 -v
go test . -run '^TestRun_retriesBlobPutAfterLostAck$' -count=1 -v
go test . -run '^TestRun_authenticatedRestartRepairsAndDeduplicatesContentBlob$' -count=1 -v
P2P_ADDR=127.0.0.1:17070 HEALTH_ADDR=127.0.0.1:18080 make demo-replication
make demo-compose
make demo-auth
make demo-repair
make demo-failure
make demo-continuation
go run . -version  # expected: 0.13.0
```

## Version

1. Update [internal/version/version.go](../internal/version/version.go) to the release semver.
2. Move completed [CHANGELOG.md](../CHANGELOG.md) entries from `[Unreleased]` into `[MAJOR.MINOR.PATCH] - YYYY-MM-DD`; retain a truthful `[Unreleased]` section for follow-up work.
3. Commit the version bump:

```bash
git add internal/version/version.go CHANGELOG.md README.md docs/RELEASE.md
git commit -m "chore(release): cut v0.13.0"
```

## Tag

```bash
git tag -a v0.13.0 -m "v0.13.0"
git push origin main
git push origin v0.13.0
```

## Release Notes

Highlight:

- Bounded whole-inventory anti-entropy exchanges with cursor continuations, indexed store paging, and live-cursor fallback coverage.
- Opt-in lifecycle records with durable authority allocation, verified raw-blob preflight, bounded journals, snapshots, and per-peer repair watermarks.
- Capability-gated lifecycle repair sessions with ordered batches, tombstone preservation, duplicate-safe replay, reconnect resume, and raw-only mixed-version compatibility.
- Operator-authored membership fences and checkpoint-first compaction that never physically deletes raw blobs.
- Negotiated startup-watermark reconciliation and stale-peer snapshot recovery after lifecycle metadata loss.
- Authenticated three-node Compose acceptance coverage for present records, tombstones, source restart, checkpoint restoration, and retained raw bytes.
- Aggregate lifecycle status and Prometheus gauges for readiness, journal state, repair sessions, membership progress, and compaction safety without peer or logical-key labels.
- Aggregate inventory, repair, continuation, scheduler, and resource-budget metrics exposed through deterministic JSON and Prometheus health endpoints.
- Finite peer admission and overload behavior, authenticated application identities, exact inbound identity allowlists, and tokenless compatibility.
- Library and CLI TLS/mTLS admission with bounded handshakes, credential validity/expiry health, and restart-only certificate rotation semantics.
- Typed Prometheus exposition with deterministic metadata, bounded health HTTP resources, and cancellation-safe graceful server shutdown.
- Concrete staged `TCPTransport.Drain(ctx)` with admission quiescence, tracked-work joins, deadline force-close, and compatibility-preserving `Close()`.
- CLI-owned `-shutdown-grace` coordination that stops new scheduler/reconnect work, shuts down health, and drains P2P transport without a new wire message.
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
- Anti-entropy inventory benchmark and the decision to retain the bounded flat `blob.has` protocol for mixed-version compatibility.
- Dependency updates for checkout, setup-go, golangci-lint, testify, and upload-artifact with green CI evidence.

Attach or link the CI SBOM artifact when publishing GitHub release binaries.
