# Deployment

## Container

Build and run (example flags):

```bash
docker build -t streamhive:local .
docker run --rm -p 7070:7070 -p 8080:8080 streamhive:local \
  -listen 0.0.0.0:7070 \
  -health 0.0.0.0:8080
```

- **7070** — P2P TCP listener (example).
- **8080** — HTTP `/livez`, `/readyz`, `/version` (aggregate runtime identity), `/peers` (JSON peer metadata), `/metrics` (JSON counters), `/metrics/prometheus` (Prometheus text), `/inventory/status` (aggregate live key fingerprint), `/storage/status` (aggregate live blob integrity), and `/lifecycle/status` (aggregate opt-in lifecycle state).

Use TLS flags (`-tls-cert`, `-tls-key`, `-tls-ca`, `-tls-server-name`) when exposing services beyond a lab network. The verified certificate and application-auth ordering is documented in [TLS_AUTH.md](TLS_AUTH.md) and exercised by `make test-tls-auth`. Reserve `-tls-insecure-skip-verify` for local development. For CLI mTLS, add `-tls-client-ca -tls-require-client-cert` on the listener and `-tls-client-cert -tls-client-key` on outbound peers. Use `-tls-expiry-warning` to set the aggregate short-lived-credential warning window (`720h` by default, `0` disables the warning). For custom trust policy, configure `p2p.TCPTransport.TLSServerConfig` and `TLSClientConfig` in library code.

Certificate files are loaded at process startup. Use the [TLS rotation runbook](TLS_ROTATION.md)
for replacement material, trust overlap, restart ordering, rollback, and reconnect checks.
StreamHive does not promise hot reload or live certificate changes for active connections.
Configured leaf identities are parsed and checked for `NotBefore`/`NotAfter` before listener
readiness. `/readyz` becomes unavailable if a configured identity later expires; CA bundles are
not included in the identity count or expiry timestamp.

For a long-lived node, `-peer-reconnect` retries only the comma-separated `-peers` targets with
the configured exponential backoff; `-dial` remains a one-shot attempt. The health metrics
`peer_reconnect_targets`, `peer_reconnect_active`, `peer_reconnect_attempts`,
`peer_reconnect_failures`, and `peer_reconnect_successes` show configured retry targets, live retry
loops, attempts, non-shutdown failures, and successful connections without target or error labels.
Concrete TCP outbound peers retain the exact configured `-peers` target, so hostname targets remain
reconnectable after a connection reports its resolved numeric address on disconnect. Use the active
gauge and failure counter for alerts; reconnect activity does not change `/readyz`.

For private clusters where every node shares an operator-managed secret, add
`-peer-auth-token` to each node. Add a stable `-peer-id` to make the remote application
visible in `/peers` and connection logs. Add `-peer-allow-ids` to each listener when only
specific remote identities should be admitted; matching is exact and missing identities
are rejected. Peers that do not present the token are rejected before replication frames
reach the application handler. Identity labels and allowlists do not replace TLS/mTLS;
keep the P2P port behind a trusted network boundary and use TLS/mTLS when the token or
identity leaves localhost.

## Docker Compose demo

Run a local 3-node cluster:

```bash
make demo-compose
```

The demo builds `streamhive:local`, starts node1, seeds one blob, starts node2 and node3, verifies node3 receives the blob, wipes node3's local demo data, restarts node3, and verifies startup anti-entropy rehydrates the blob again.

Run the token-protected Compose acceptance demo:

```bash
STREAMHIVE_PEER_TOKEN="replace-with-a-local-demo-token" make demo-auth
```

The same optional token is passed to every Compose node and the seed tool. The demo verifies
the expected remote identities, rejects an unlisted identity and a different token before
storage, restarts node3 with an empty durable directory, and verifies periodic anti-entropy
rehydrates the exact content-addressed key. The final output is a compact audit summary.

Run the authenticated lifecycle compaction proof:

```bash
STREAMHIVE_LIFECYCLE_TOKEN="replace-with-a-local-demo-token" make test-lifecycle-compose
```

This separate topology keeps node1 as the lifecycle source and gives node2 and node3 durable
stores, journals, checkpoints, watermarks, and operator-authored membership. It seeds one present
record and one tombstone, waits for both acknowledgements, compacts and restarts node1, then
restarts node3 after removing only its lifecycle directory. The demo requires node3 to restore the
checkpointed logical state and retain the expected raw SHA-256 blob, with bounded health polling
and Compose cleanup on every exit path.

Inspect a running Compose cluster:

```bash
make demo-status
```

The status command prints each node's `/peers`, `/metrics`, and durable store keys.

`/metrics/prometheus` is deterministic and scrape-ready: every `streamhive_*` sample is preceded
by one `# HELP` and one `# TYPE` line, output is sorted by metric name, and no labels are emitted.
The existing JSON keys and values remain unchanged. Run `make test-prometheus-format` when
validating a monitoring integration or exporter change.

The optional health HTTP server uses a bounded resource envelope: 5 seconds for request headers,
10 seconds for request reads and response writes, 60 seconds for idle keep-alive connections, and
1 MiB maximum request headers. Health handlers return small finite JSON/text responses, accept only
`GET` and `HEAD`, and reject other methods with `405 Method Not Allowed` plus `Allow: GET, HEAD`
before endpoint work begins. The server performs graceful shutdown when the process context is
canceled; slow or oversized clients must not hold the health listener indefinitely.

The CLI `-shutdown-grace` flag defaults to 3 seconds and is the single deadline owner for
normal application shutdown. Cancellation stops new scheduler and reconnect work, health
shutdown runs first within the remaining budget, and the P2P transport then drains cooperatively
before force-closing at expiry. Use a larger value when frame handlers or health clients need
more time; fatal startup failures and successful one-shot completion still use immediate cleanup.
The acceptance target is `make test-cli-shutdown`.

Run the corruption repair demo:

```bash
make demo-repair
```

The repair demo starts the same 3-node cluster, seeds one content-addressed blob, overwrites node3's durable bytes, and verifies FileStore detects the mismatch and periodic anti-entropy restores the exact SHA-256 content.

Run the reconnect/failure demo:

```bash
make demo-failure
```

The failure demo starts the same 3-node cluster, seeds a blob, stops node2, deletes node2's durable blob file while the process is down, restarts node2, waits for `/peers` to show an active connection, and verifies periodic anti-entropy restores the key.

For a startup-only contract check that does not rely on periodic inventory, run:

```bash
make test-eviction-repair
```

The acceptance path first converges a durable target, evicts its local content-addressed blob
while stopped, restarts with `-sync-interval 0s`, and verifies startup anti-entropy restores the
exact bytes. A successful repair means local eviction is rehydratable; it does not make
`FileStore.Delete` a distributed logical deletion.

Run the bounded continuation demo:

```bash
make demo-continuation
```

This starts two local processes, seeds three 4-byte blobs, limits the source to 8 repair
bytes per response, leaves periodic inventory disabled, and checks that the continuation
counters and target storage prove the deferred blob arrived.

Run the bounded inventory convergence demo:

```bash
make demo-inventory-budget
```

This starts two real TCP processes with durable stores, seeds eight SHA-256-addressed blobs,
limits each startup inventory exchange to one key and 128 encoded bytes, stops the target while
the source has a pending cursor, restarts the target, and verifies every key plus JSON and
Prometheus counters. The demo is cleanup-safe and accepts `P2P_ADDR`, `HEALTH_ADDR`,
`TARGET_P2P_ADDR`, `TARGET_HEALTH_ADDR`, and `STREAMHIVE_DATA_DIR` overrides.

For a multi-peer fairness check that also exercises source mutation, run:

```bash
make test-inventory-fairness
```

This uses temporary durable stores and five race-enabled repetitions. It disconnects one target
while a second target continues, and deletes only a source key that has not been advertised yet;
deletion propagation remains out of scope until the protocol has an explicit tombstone message.

Health endpoints are exposed on:

- **node1**: <http://127.0.0.1:18081>
- **node2**: <http://127.0.0.1:18082>
- **node3**: <http://127.0.0.1:18083>

## Kubernetes (minimal)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: streamhive
spec:
  replicas: 1
  selector:
    matchLabels:
      app: streamhive
  template:
    metadata:
      labels:
        app: streamhive
    spec:
      containers:
        - name: streamhive
          image: streamhive:local
          args: ["-listen", "0.0.0.0:7070", "-health", "0.0.0.0:8080"]
          ports:
            - containerPort: 7070
              name: p2p
            - containerPort: 8080
              name: health
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /livez
              port: health
            initialDelaySeconds: 2
            periodSeconds: 10
```

Add a `Service` for the health port and (separately) headless or load-balanced service for P2P depending on your topology. Tune resource requests/limits and pod anti-affinity for HA; this manifest is illustrative only.

## SLOs

Define error budgets once you expose a workload to users. Baseline probes:

- **Availability**: `/livez` success rate.
- **Readiness**: `/readyz` reflects a bound listener and currently valid configured TLS identities; without TLS credentials it retains listener-only behavior.
- **Runtime identity**: `/version` returns only the semver, `streamhive/1` handshake version, and `SHV1` framing magic, with no peer, credential, filesystem, host, or commit metadata.
- **TLS credential health**: `tls_certificates_configured`, `tls_certificate_expiry_timestamp_seconds`, `tls_certificates_expired`, `tls_certificates_not_yet_valid`, and `tls_certificates_expiring_soon` expose aggregate leaf-identity health through JSON and Prometheus without certificate or peer labels.
- **Static peer reconnect**: `peer_reconnect_targets`, `peer_reconnect_active`, `peer_reconnect_attempts`, `peer_reconnect_failures`, and `peer_reconnect_successes` expose retry pressure and recovery without target, address, credential, or error labels. A disconnect during an active target loop queues one coalesced follow-up retry, while shutdown cancellation is excluded from failures; reconnect state does not make `/readyz` depend on remote peer availability.
- **Prometheus exposition**: `/metrics/prometheus` emits one sorted `# HELP`/`# TYPE` pair per sample. Event counters are typed `counter`; active, pending, in-flight, queued, and TLS health values are typed `gauge`.
- **Lifecycle compaction**: `lifecycle_membership_configured`, `lifecycle_membership_members`, `lifecycle_membership_acknowledged`, `lifecycle_membership_min_epoch`, `lifecycle_membership_min_sequence`, `lifecycle_compaction_target_epoch`, `lifecycle_compaction_target_sequence`, and `lifecycle_compaction_blocked` expose the bounded operator fence and progress without member or peer labels. A missing membership file blocks `-lifecycle-compact`; an explicitly persisted empty membership is valid for a standalone node. Run `make test-lifecycle-compaction` for the focused real-TCP proof or `make test-lifecycle-compose` for the authenticated three-node checkpoint recovery proof.
- **Health HTTP resource envelope**: request headers are bounded at 5 seconds and 1 MiB, reads/writes at 10 seconds, and idle connections at 60 seconds; cancellation closes the listener through graceful server shutdown.
- **Peer visibility**: `/peers` returns active connected peers with remote address, local address, direction, connection timestamp, connection age, `auth_method` (`none` or `shared-token`), and optional `auth_identity`.
- **Inventory visibility**: `/inventory/status` returns only the live-scan marker, key count, key-byte total, and length-prefixed SHA-256 fingerprint. It does not expose blob keys, content, peer labels, or deletion state; equal fingerprints are a convergence signal, not proof of a snapshot.
- **Inventory scan accounting**: `replication_inventory_status_scans_started`, `replication_inventory_status_scans_completed`, and `replication_inventory_status_scans_failed` count endpoint attempts; `replication_inventory_status_keys_scanned` and `replication_inventory_status_key_bytes_scanned` count successful observations; `replication_inventory_status_scan_duration_ms` accumulates attempt time. They are available in JSON and Prometheus without labels, and raw-only nodes leave them at zero.
- **Storage integrity visibility**: `/storage/status` verifies 32-byte content-addressed keys against current bytes and reports aggregate verified, opaque, corrupt, and missing counts plus byte totals. `replication_storage_integrity_*` counters record attempts, outcomes, observed classes, bytes, and duration without labels. The result is a live diagnostic, not a snapshot, readiness decision, deletion signal, or repair command; raw-only nodes report a disabled zero state. See [STORAGE_INTEGRITY.md](STORAGE_INTEGRITY.md).
- **Offline storage preflight**: `go run . -store-dir /var/lib/streamhive/blobs -verify-store` runs the same aggregate integrity scan without opening a listener or requiring `-replicate`. It prints status-compatible JSON and exits non-zero when corruption or missing entries make the result unhealthy, or when malformed regular names or unreadable entries prevent enumeration; it never mutates the store. FileStore ignores temporary and other non-regular entries.
- **Storage mutation cancellation**: `MemoryStore` and `FileStore` honor canceled contexts while waiting for mutation serialization and before the data/index commit boundary. A cancellation after a rename, remove, or in-memory commit has begun does not claim to roll the mutation back.
- **Storage mutation durability**: `FileStore` syncs temporary blob contents before atomic replacement and syncs the store directory after successful puts and deletes. This is a local filesystem durability boundary for process restart and crash recovery, not a replicated acknowledgement or hardware guarantee; a sync error after mutation begins is returned without claiming rollback.
- **Saturation/auth/replication**: JSON `/metrics` fields `active_peers`, `peers_rejected`, `peer_auth_success`, `peer_auth_failures`, `peer_auth_identity_rejections`, `replication_blob_acks_sent`, `replication_blob_acks_received`, `replication_blob_acks_matched`, `replication_blob_ack_timeouts`, `replication_blob_retries`, `replication_blob_acks_pending`, `replication_blob_puts_accepted`, `replication_blob_put_failures`, `replication_blob_write_errors`, `replication_inventory_advertisements`, `replication_inventory_bytes_sent`, `replication_inventory_keys_sent`, `replication_inventory_keys_probed`, `replication_inventory_exchanges_started`, `replication_inventory_exchanges_completed`, `replication_inventory_exchanges_limited`, `replication_inventory_exchanges_active`, `replication_missing_keys_requested`, `replication_repair_blobs_sent`, `replication_repair_blobs_deferred`, and `replication_corrupt_blobs_detected`, or Prometheus samples from `/metrics/prometheus`. The anti-entropy counters are aggregate: advertisements count successful `blob.has` frames, keys sent count advertised keys, missing-key requests count keys in `blob.missing` messages, exchange-limited counts budgeted continuations, repair sends count successful repair `blob.put` frames, deferred counts keys held back by the per-response repair budget, and corruption counts damaged content-addressed files replaced by a verified repair.

`replication_inventory_exchanges_dropped` counts canceled or failed inventory scheduler
work without peer labels.

- **Content integrity**: `replication_corrupt_blobs_detected` also counts mismatched bytes found
  while reading a generic repair source. Those bytes are skipped and never emitted as an
  unverified `blob.put`; the counter remains aggregate and label-free.

Continuation counters are also aggregate: `replication_repair_continuations_scheduled`,
`replication_repair_continuations_completed`, and
`replication_repair_continuations_dropped` count queued, executed, and discarded
continuation batches without peer labels. The
`replication_repair_continuations_active` gauge counts scheduled or running per-peer
continuation entries, while `replication_repair_continuation_keys_pending` includes
queued and currently in-flight keys. Both gauges are zero when no continuation work is
outstanding and remain bounded by the scheduler's per-peer queue limit plus the active
batch.

Set `-max-repair-ops` to cap concurrent anti-entropy blob reads/writes across all peers;
the default is four and `0` selects that default. Observe the aggregate
`replication_repair_io_ops_started`, `replication_repair_io_ops_completed`,
`replication_repair_io_ops_waited`, `replication_repair_io_ops_rejected`,
`replication_repair_io_ops_in_flight`, and `replication_repair_io_ops_queued` metrics.
These metrics have no peer or blob labels.

Set `-max-inventory-bytes` and `-max-inventory-keys` to bound each peer's complete
anti-entropy inventory exchange. Defaults are 16 MiB of encoded payload and 16,384 keys;
`0` disables the corresponding cap. A budget hit resumes from an exclusive cursor after
a short delay, and periodic inventory remains the convergence fallback.
