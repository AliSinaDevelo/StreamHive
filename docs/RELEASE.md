# Release Checklist

Use this checklist for StreamHive releases. The current target is `v0.7.0`, the first release with richer peer metadata, protocol/security documentation, and partial-sync send resilience.

## Preflight

```bash
go test ./...
go test -bench=. -benchmem -run '^$' ./...
P2P_ADDR=127.0.0.1:17070 HEALTH_ADDR=127.0.0.1:18080 make demo-replication
make demo-compose
make demo-repair
make demo-failure
go run . -version
```

## Version

1. Update [internal/version/version.go](../internal/version/version.go) to the release semver.
2. Move completed [CHANGELOG.md](../CHANGELOG.md) entries from `[Unreleased]` into `[MAJOR.MINOR.PATCH] - YYYY-MM-DD`.
3. Commit the version bump:

```bash
git add internal/version/version.go CHANGELOG.md README.md docs/RELEASE.md
git commit -m "chore: release v0.7.0"
```

## Tag

```bash
git tag -a v0.7.0 -m "v0.7.0"
git push origin main
git push origin v0.7.0
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

Attach or link the CI SBOM artifact when publishing GitHub release binaries.
