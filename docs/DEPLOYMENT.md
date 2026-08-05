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
- **8080** — HTTP `/livez`, `/readyz`, `/peers` (JSON peer metadata), `/metrics` (JSON counters), `/metrics/prometheus` (Prometheus text).

Use TLS flags (`-tls-cert`, `-tls-key`, `-tls-ca`, `-tls-server-name`) when exposing services beyond a lab network. Reserve `-tls-insecure-skip-verify` for local development. For mTLS or custom trust policy, configure `p2p.TCPTransport.TLSServerConfig` and `TLSClientConfig` in library code.

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

Inspect a running Compose cluster:

```bash
make demo-status
```

The status command prints each node's `/peers`, `/metrics`, and durable store keys.

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
- **Readiness**: `/readyz` reflects listener bound (`TCPTransport.Ready`).
- **Peer visibility**: `/peers` returns active connected peers with remote address, local address, direction, connection timestamp, connection age, `auth_method` (`none` or `shared-token`), and optional `auth_identity`.
- **Saturation/auth/replication**: JSON `/metrics` fields `active_peers`, `peers_rejected`, `peer_auth_success`, `peer_auth_failures`, `peer_auth_identity_rejections`, `replication_blob_acks_sent`, `replication_blob_acks_received`, `replication_blob_acks_matched`, `replication_blob_ack_timeouts`, `replication_blob_retries`, `replication_blob_acks_pending`, `replication_blob_puts_accepted`, `replication_blob_put_failures`, `replication_blob_write_errors`, `replication_inventory_advertisements`, `replication_inventory_bytes_sent`, `replication_inventory_keys_sent`, `replication_inventory_keys_probed`, `replication_inventory_exchanges_started`, `replication_inventory_exchanges_completed`, `replication_inventory_exchanges_limited`, `replication_inventory_exchanges_active`, `replication_missing_keys_requested`, `replication_repair_blobs_sent`, `replication_repair_blobs_deferred`, and `replication_corrupt_blobs_detected`, or Prometheus samples from `/metrics/prometheus`. The anti-entropy counters are aggregate: advertisements count successful `blob.has` frames, keys sent count advertised keys, missing-key requests count keys in `blob.missing` messages, exchange-limited counts budgeted continuations, repair sends count successful repair `blob.put` frames, deferred counts keys held back by the per-response repair budget, and corruption counts damaged content-addressed files replaced by a verified repair.

`replication_inventory_exchanges_dropped` counts canceled or failed inventory scheduler
work without peer labels.

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
