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
`-peer-auth-token` to each node. Peers that do not present the token are rejected before
replication frames reach the application handler. This is shared-token admission control,
not per-peer identity or authorization; keep the P2P port behind a trusted network
boundary and use TLS/mTLS when the token leaves localhost.

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

The same optional token is passed to every Compose node and the seed tool. The demo also
attempts a write with a different token and verifies that the peer is rejected and the
unauthorized key is absent from node1's durable store.

Inspect a running Compose cluster:

```bash
make demo-status
```

The status command prints each node's `/peers`, `/metrics`, and durable store keys.

Run the corruption repair demo:

```bash
make demo-repair
```

The repair demo starts the same 3-node cluster, seeds one content-addressed blob, deletes node3's durable blob file, and verifies periodic anti-entropy restores the exact key.

Run the reconnect/failure demo:

```bash
make demo-failure
```

The failure demo starts the same 3-node cluster, seeds a blob, stops node2, deletes node2's durable blob file while the process is down, restarts node2, waits for `/peers` to show an active connection, and verifies periodic anti-entropy restores the key.

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
- **Peer visibility**: `/peers` returns active connected peers with remote address, local address, direction, connection timestamp, connection age, and `auth_method` (`none` or `shared-token`).
- **Saturation/auth/replication**: JSON `/metrics` fields `active_peers`, `peers_rejected`, `peer_auth_success`, `peer_auth_failures`, `replication_blob_acks_sent`, `replication_blob_acks_received`, `replication_blob_acks_matched`, `replication_blob_ack_timeouts`, `replication_blob_retries`, and `replication_blob_acks_pending`, or Prometheus samples from `/metrics/prometheus`.
