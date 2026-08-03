# Release Checklist

Use this checklist for StreamHive releases. The current target is `v0.9.0`, the release with exchanged peer identities, exact inbound identity allowlists, and Compose acceptance evidence.

## Preflight

```bash
go test ./...
go test -bench=. -benchmem -run '^$' ./...
go test . -run '^TestRun_retriesBlobPutAfterLostAck$' -count=1 -v
P2P_ADDR=127.0.0.1:17070 HEALTH_ADDR=127.0.0.1:18080 make demo-replication
make demo-compose
make demo-auth
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
git commit -m "chore: release v0.9.0"
```

## Tag

```bash
git tag -a v0.9.0 -m "v0.9.0"
git push origin main
git push origin v0.9.0
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

Attach or link the CI SBOM artifact when publishing GitHub release binaries.
