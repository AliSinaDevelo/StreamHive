# Changelog

All notable changes to StreamHive are documented here. This project follows [Semantic Versioning](https://semver.org/) for the **public Go API** (`p2p`, `storage`, and stable CLI flags). Until v1.0.0, minor releases may include API adjustments; see entries below.

## [Unreleased]

### Added

- **Replication**: aggregate `-max-inventory-bytes` and `-max-inventory-keys` budgets bound whole anti-entropy exchanges per peer; delayed continuations resume from an exclusive cursor without changing the flat `blob.has` wire format.
- **Metrics**: inventory keys sent and exchange started/completed/limited/dropped/active counters expose aggregate scheduler pressure without peer, blob, or key labels.
- **Tests**: deterministic inventory budget tests cover byte/key saturation, cursor continuation, cancellation, peer independence, and budgeted end-to-end benchmarks.
- **Tests**: real-TCP budgeted inventory acceptance coverage proves eight content-addressed blobs converge after a target disconnect and durable restart, with JSON and Prometheus counter assertions.
- **Demo**: `make demo-inventory-budget` reproduces the interrupted startup exchange with cleanup-safe local processes and fixed-port CI defaults.
- **Tests**: multi-peer mutation coverage exercises two durable targets, exclusive cursor refresh after a source mutation, disconnect fairness, and exact final key sets under five race-enabled repetitions.
- **CI**: `make test-inventory-fairness` runs as a dedicated Go 1.23.x pipeline job.
- **Tests**: startup-only local eviction acceptance coverage proves a durable target rehydrates an evicted SHA-256 blob without periodic inventory or a delete message.
- **CI**: `make test-eviction-repair` runs five race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **Tests**: live-cursor acceptance coverage proves a key inserted behind an active cursor is repaired by the next periodic inventory pass.
- **CI**: `make test-inventory-consistency` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **Docs**: `docs/INVENTORY_CONSISTENCY.md` defines live cursor versus snapshot semantics and records the periodic fallback research boundary.
- **Tests**: real-TCP peer admission coverage proves a one-peer cap rejects an excess client while the admitted peer remains active.
- **CI**: `make test-peer-admission` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **Docs**: `docs/PEER_ADMISSION.md` defines the explicit finite production cap and preserves the unlimited compatibility default.
- **Tests**: real-TCP TLS acceptance coverage generates ephemeral certificates, proves hostname and CA verification plus exact application identity admission, and checks wrong-CA and wrong-hostname failures before peer registration.
- **CI**: `make test-tls-auth` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **Docs**: `docs/TLS_AUTH.md` defines the transport certificate, shared-token, identity-allowlist, and library mTLS boundaries.
- **`p2p`**: inbound TLS handshakes complete before peer registration, `OnPeer`, and frame handling, with a bounded `TLSHandshakeTimeout`.
- **Metrics**: aggregate `tls_handshake_success` and `tls_handshake_failures` expose local TLS handshake outcomes without certificate or address labels.
- **Tests**: library mTLS acceptance coverage proves verified frame exchange and pre-registration rejection for missing or unrelated client certificates.
- **CI**: `make test-mtls` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **Tests**: restart-only TLS rotation coverage proves certificate replacement, static-peer reconnect, malformed startup rejection, rollback, and aggregate metrics over real TCP.
- **CI**: `make test-tls-rotation` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **CLI**: `-tls-client-cert` / `-tls-client-key` present outbound client certificates, while `-tls-client-ca` / `-tls-require-client-cert` enable strict inbound mTLS verification with startup validation.
- **Tests**: CLI mTLS acceptance coverage proves trusted client admission, missing/untrusted rejection before peer registration, and fail-closed flag validation.
- **CI**: `make test-mtls-cli` runs three race-enabled repetitions as a dedicated Go 1.23.x pipeline job.
- **CLI**: `-tls-expiry-warning` reports a bounded warning window for configured server and outbound client identities; expired or not-yet-valid leaf certificates fail before listener readiness.
- **Metrics**: aggregate TLS identity configuration, earliest expiry, expired, not-yet-valid, and expiring-soon values are available through JSON and Prometheus without certificate labels.
- **Tests**: `make test-tls-credential-health` covers valid dual identities, warning suppression, invalid startup credentials, readiness, and aggregate metric output under the race detector.
- **CI**: `make test-tls-credential-health` runs as a dedicated Go 1.23.x pipeline job.
- **Metrics**: `/metrics/prometheus` now emits deterministic `# HELP` and `# TYPE` metadata for every label-free health sample while preserving existing names and values.
- **Tests**: `make test-prometheus-format` parses the complete transport/replication/TLS exposition and verifies metadata pairing, safe names, ordering, and counter/gauge classification.
- **CI**: `make test-prometheus-format` runs as a dedicated Go 1.23.x pipeline job.
- **Docs**: `docs/TLS_ROTATION.md` defines restart-only certificate rotation, session-resumption boundaries, rollback, observability, and the focused follow-up acceptance plan.
- **Docs**: `docs/DELETION_SEMANTICS.md` defines local blob eviction versus future logical deletion and records the tombstone/versioning research boundary.
- **Metrics**: aggregate repair-continuation active and pending-key gauges expose scheduler saturation through JSON and Prometheus without peer or blob labels.
- **Tests**: bounded fuzz smoke targets cover replication decode/base64/limit validation and SHV1 frame length/magic boundaries, including valid round trips.
- **Storage**: durable 32-byte SHA-256 keys are verified on read and inventory; corrupted content is observable and repairable through anti-entropy.
- **Storage**: `BlobKeyPager` lets native memory and file stores enumerate bounded inventory pages with cancellation and deterministic bytewise cursors; legacy `BlobKeyLister` implementations remain compatible.
- **Storage**: `MemoryStore` and `FileStore` maintain ordered B-tree key indexes for cursor pages; FileStore rebuilds from durable filenames when its directory stamp changes without adding a sidecar format.
- **Metrics**: aggregate anti-entropy inventory bytes sent and receiver key probes are exposed without peer, blob, or key labels.
- **Tests**: an end-to-end indexed inventory exchange benchmark covers 4,096/65,536 keys and 32/64/512-byte key widths before any wire-format change.
- **Replication**: `-max-repair-ops` applies a process-wide default-four admission budget to anti-entropy blob reads/writes, with aggregate queue, in-flight, wait, completion, and rejection metrics.
- **Docs**: resource-budget design records peer, frame, blob, repair, reconnect, shutdown, and paged-inventory pressure behavior, with deterministic saturation and inventory benchmarks.

## [0.11.0] — 2026-08-05

### Added

- **Replication**: bounded anti-entropy repair responses with `-max-repair-bytes` and later-inventory recovery for deferred keys.
- **Replication**: one bounded delayed continuation merges deferred repair keys per peer while preserving periodic inventory as the fallback.
- **Metrics**: `replication_repair_blobs_deferred` counts keys held back by the per-response repair budget.
- **Metrics**: continuation scheduling, completion, and dropped-work counters are exposed through JSON and Prometheus health endpoints.
- **Demo**: `make demo-continuation` forces a small repair budget and proves continuation delivery without periodic inventory.
- **Tests**: multi-peer continuation fairness is covered by a deterministic race-enabled acceptance target and dedicated CI matrix job.
- **Docs**: anti-entropy inventory research records the v0.11 decision to keep the bounded flat `blob.has` protocol until scale measurements justify a versioned digest exchange.

## [0.10.0] — 2026-08-04

### Added

- **Metrics**: aggregate anti-entropy counters for inventory advertisements, missing-key requests, and repair blob deliveries.
- **Ops**: structured replication logs distinguish one-shot sends from anti-entropy repair deliveries without adding peer labels.
- **Demo**: authenticated Compose output now summarizes identity authorization, rejection counters, and restart repair evidence.
- **Tests**: real TCP coverage combines authenticated identity policy, restart repair, and exact duplicate replay.

## [0.9.0] — 2026-08-03

### Added

- **`p2p` / CLI**: optional application identity exchange during shared-token auth via `PeerAuthIdentity` and `-peer-id`.
- **`p2p` / CLI**: exact inbound identity allowlists via `PeerAuthAllowedIdentities` and `-peer-allow-ids`.
- **Ops**: `/peers` snapshots and connection logs expose optional `auth_identity` values.
- **Metrics**: `peer_auth_identity_rejections` counts inbound identities rejected by validation or the allowlist.

## [0.8.0] — 2026-08-03

### Added

- **`p2p` / CLI**: optional shared-token peer auth handshake via `TCPTransport.PeerAuthToken` and `-peer-auth-token`.
- **`replication`**: `blob.ack` messages acknowledge accepted `blob.put` keys.
- **CLI**: one-shot blob puts wait for matching acknowledgments and retry idempotently with `-put-ack-timeout`, `-put-retries`, and `-put-retry-delay`.
- **Metrics**: `peer_auth_success` and `peer_auth_failures` counters for authenticated peer admission.
- **Metrics**: `replication_blob_acks_sent` and `replication_blob_acks_received` counters.
- **Metrics**: `replication_blob_acks_matched`, `replication_blob_acks_pending`, `replication_blob_ack_timeouts`, and `replication_blob_retries` counters.
- **Metrics**: `replication_blob_puts_accepted`, `replication_blob_put_failures`, and `replication_blob_write_errors` counters.
- **Demo**: authenticated Docker Compose acceptance path proving matching-token replication and wrong-token rejection.
- **Tests**: real TCP acceptance coverage drops the first blob ACK and verifies duplicate-safe retry completion.
- **Ops**: peer snapshots and connection logs label admission as `auth_method=none` or `auth_method=shared-token`.

### Changed

- **`p2p`**: concurrent `TCPPeer.WriteFrame` calls are serialized per connection to preserve frame boundaries.
- **Ops**: one-shot delivery logs classify accepted, ACK-timeout, write-error, and canceled outcomes; write failures close the peer.

## [0.7.0] — 2026-08-02

### Added

- **`p2p`**: `PeerSnapshots` exposes connected peer metadata for operational tooling.
- **Docs**: protocol reference for SHV1 frames, replication messages, limits, and repair behavior.
- **Docs**: TLS/mTLS peer identity guidance and remaining application-level auth gaps.
- **Metrics**: `replication_blobs_skipped` counts requested blobs that could not be sent without aborting the peer loop.

### Changed

- **Ops**: `/peers` now includes local address, connection timestamp, and connection age.
- **Replication**: requested blob sends now skip unreadable or oversized individual blobs and continue sending remaining requested keys.

## [0.6.0] — 2026-07-02

### Added

- **Ops**: `/peers` JSON endpoint for inspecting active peer addresses and connection direction.
- **Demo**: `make demo-status` prints each Compose node's peers, metrics, and durable keys.
- **Demo**: `make demo-failure` proves reconnect plus anti-entropy repair after a node restart.
- **CI**: Docker Compose reconnect/failure demo verification.

## [0.5.0] — 2026-07-01

### Added

- **Metrics**: duplicate blob counters for idempotent replication receives.
- **CLI**: `-sync-interval` for periodic anti-entropy inventory after peer startup.
- **Demo**: 3-node corruption repair demo for deleted durable blobs.
- **CI**: Docker Compose corruption repair demo verification.

### Changed

- **CLI**: `blob.put` handling now skips exact duplicate key/data writes while still allowing opaque-key replacement.
- **CLI**: SHA-256-shaped blob keys are verified against received data before storage.

## [0.4.0] — 2026-07-01

### Added

- **`storage`**: SHA-256 content key helpers for content-addressed blob IDs.
- **`storage`**: `BlobKeyLister` interface plus deterministic `ListKeys` support for memory and file stores.
- **`replication`**: `blob.has`, `blob.get`, and `blob.missing` message types for anti-entropy sync.
- **CLI**: startup anti-entropy sync for `-replicate` peers using `blob.has` / `blob.missing` / `blob.put`.
- **CLI**: `-put-content-key` for sending blobs under `SHA-256(-put-data)` content keys.
- **CLI**: `-list-keys` for inspecting durable `-store-dir` keys as hex.
- **Demo**: 3-node Docker Compose demo with durable stores and node restart rehydration.
- **Metrics**: Prometheus text endpoint at `/metrics/prometheus`.
- **CI**: Docker Compose rehydration demo verification.

## [0.3.0] — 2026-06-24

### Added

- **`replication`**: typed blob replication protocol with `blob.put` encoding, decoding, validation limits, and `BlobStore` apply helper.
- **`storage`**: `FileStore` directory-backed `BlobStore` with hex-encoded keys and restart persistence.
- **`p2p`**: `TCPPeer.WriteFrame` convenience method for framed peer writes.
- **CLI**: `-replicate`, `-store-dir`, `-put-key`, `-put-data`, `-exit-after-put`, `-peers`, `-peer-reconnect`, `-peer-reconnect-min`, `-peer-reconnect-max`, and `-max-blob-bytes` for static replication demos and long-lived static-peer nodes.
- **Metrics**: replication counters for stored/sent blobs, stored/sent bytes, and replication errors.
- **Demo**: `make demo-replication` starts a receiver, sends one blob, waits for metrics, and prints the evidence.

## [0.2.0] — 2026-04-05

### Added

- **Semver** `Version` constant and this changelog.
- **`storage`**: `BlobStore` interface and in-memory implementation for content-keyed blobs.
- **`p2p`**: length-prefixed wire framing (`SHV1` magic), metrics, `context.Context` on `ListenAndAccept` / `Dial`, graceful accept-loop shutdown, peer removal on disconnect, optional max peers, dial / read idle deadlines, optional TLS, optional framed `FrameHandler`.
- **CLI**: `-version`, `-max-peers`, `-dial-timeout`, `-read-idle-timeout`, optional `-health` (live/ready/metrics), optional TLS flags.
- **Ops docs**: deployment (Docker/K8s sketch), governance (branch protection checklist), SBOM artifact in CI, pinned GitHub Actions by commit SHA.

### Changed

- **Breaking**: `Transport.ListenAndAccept` and `Dial` now take `context.Context` as the first argument.

## [0.1.0]

- Initial public foundation: TCP transport, tests, CI, documentation.
