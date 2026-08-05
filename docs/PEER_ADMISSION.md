# Peer Admission

Status: v0.12 operator contract, tracked by issue #36.

## Decision

`-max-peers` is the explicit process-level admission control for connected TCP peers.

- `-max-peers 0` remains unlimited for compatibility with local demos and existing library use.
- Production operators should set a finite value based on the node's topology and resource budget.
- The cap applies after an inbound peer completes the configured auth handshake and before it is
  registered in the transport peer map. An unauthenticated or unauthorized peer is rejected by
  auth first; an authenticated peer over the cap is closed without registration.
- `active_peers` counts registered peers. `peers_rejected` counts cap rejections; it is an
  aggregate counter with no remote-address label.
- Admission is reject-on-saturation. StreamHive does not queue arbitrary connections or evict an
  admitted peer to make room for a later one.

The default remains unlimited because StreamHive cannot choose a safe number without knowing the
node's file-descriptor limit, memory budget, peer topology, and inventory/repair workload. The
explicit finite setting is the production safety contract until a representative deployment
profile justifies a new default.

## Per-Peer Ownership

For `P` registered peers, a node may own:

| Resource | Per-peer behavior |
| --- | --- |
| TCP connection | One live socket and transport peer object |
| Serve loop | One peer read/serve goroutine while connected |
| Auth state | Temporary handshake state until registration or rejection |
| Inventory | At most one scheduler entry and exclusive cursor |
| Repair | At most one running continuation and bounded pending keys |
| Metrics | Aggregate counters and gauges only; no peer labels |

This is a concurrency envelope, not a latency promise. The inventory and repair budgets still
bound work per peer, while `-max-repair-ops` bounds anti-entropy I/O across peers. The cap should
be chosen with headroom for the health server, outbound dialing, and normal process activity.

## Operator Workflow

Start a node with an explicit cap:

```bash
go run . -listen 127.0.0.1:7070 -health 127.0.0.1:8080 \
  -max-peers 8 -replicate -store-dir ./streamhive-data
```

Inspect the result through `/peers` and `/metrics`:

```bash
curl -fsS http://127.0.0.1:8080/peers
curl -fsS http://127.0.0.1:8080/metrics
curl -fsS http://127.0.0.1:8080/metrics/prometheus
```

When `active_peers` reaches the configured cap, new authenticated peers are closed and
`peers_rejected` increments. An admitted peer remains connected and its existing replication
work is not displaced by the rejected connection.

## Acceptance Evidence

Run the focused proof with:

```bash
make test-peer-admission
```

`TestRun_maxPeersRejectsSecondRealTCPPeer` starts one real CLI server with `-max-peers 1` and two
real TCP clients. It verifies one active peer, at least one rejection, exactly one active client,
and Prometheus samples for both gauges/counters. The test repeats under the race detector.

## Research Boundary

Resource limits should be explicit and observable. etcd documents configurable request and storage
limits rather than relying on an unlimited server envelope, and its monitoring guidance pairs
health endpoints with metrics and profiling. libp2p's resource manager treats connection and
stream limits as a host resource policy, separate from transport mechanics.

StreamHive currently needs only connection admission. A future resource-manager slice could add
per-peer bandwidth, frame, or protocol-class budgets, but it must preserve aggregate metric
cardinality and define overload behavior before implementation.

## Research Sources

- [etcd system limits](https://etcd.io/docs/v3.7/dev-guide/limit/) documents configurable request
  and storage quotas.
- [etcd monitoring](https://etcd.io/docs/v3.7/op-guide/monitoring/) documents health endpoints,
  metrics, and profiling for resource diagnosis.
- [go-libp2p resource manager](https://github.com/libp2p/go-libp2p/blob/master/p2p/host/resource-manager/README.md)
  treats connection and stream limits as explicit host resource policy.
