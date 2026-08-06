# Graceful P2P Peer Drain

Status: v0.12 design decision, researched in issue #45. This note describes the
follow-up contract; the current shutdown behavior remains unchanged until an
implementation issue is completed.

## Current behavior

`p2p.TCPTransport` has a local shutdown context and an idempotent `Close` method.
`Close` currently:

1. marks the transport closed and cancels the local shutdown context;
2. removes and closes the listener, which releases the accept loop;
3. waits for the accept loop; and
4. closes the peers present in the peer map at the start of shutdown.

The transport does not currently wait for every accepted-connection goroutine,
peer reader, frame handler, or disconnect callback. Admission also has no single
draining state checked by both inbound registration and outbound `Dial`, so a
connection racing with `Close` needs an explicit follow-up guard. `Close` cancels
transport work, but application-owned inventory, repair, ACK, and reconnect
workers use the caller's context and must be canceled by that owner.

The CLI already cancels its application context on process shutdown. Its deferred
health-server shutdown runs before the transport close, while replication waiters,
inventory/repair schedulers, and reconnect backoff observe that application
cancellation. The existing fallback for interrupted replication is a later
inventory pass or reconnect; there is no remote shutdown message.

## Decision

Use a staged, local, bounded drain. Do not add a shutdown message to the
replication protocol.

The smallest compatible API is a concrete `TCPTransport.Drain(ctx)` method. Keep
the public `Transport` interface unchanged so existing custom implementations do
not break. Keep `Close()` as the immediate, idempotent hard-stop path for callers
that need the current behavior. Both methods share one lifecycle state machine.

The caller owns the ordering between application work and transport work:

1. Cancel the application context so replication ACK waiters, inventory/repair
   schedulers, and reconnect loops stop accepting new work.
2. Call `Drain(ctx)` with a finite shutdown deadline.
3. Let the transport quiesce admissions, wait for cooperative local work, and
   force-close remaining sockets when the deadline expires.

`Drain` never waits indefinitely for a remote ACK or a remote peer. A peer that
does not finish before the deadline receives the same TCP connection close used
by the current hard-stop path. Anti-entropy and reconnect remain the recovery
mechanisms for work that was not acknowledged before shutdown.

## Lifecycle contract

The follow-up implementation should expose three internal states:

| State | New inbound admission | New outbound admission | Existing work |
| --- | --- | --- | --- |
| Open | Allowed | Allowed | Runs normally |
| Draining | Rejected and closed before registration | Rejected with `ErrTransportClosed` | Receives cancellation and may finish before the deadline |
| Closed | Rejected and closed | Rejected with `ErrTransportClosed` | No tracked work remains |

The transition to `Draining` must happen before the listener is closed and before
the active-peer snapshot is taken. The implementation must:

- reject a connection that completes authentication after draining begins;
- reject an outbound dial that completes after draining begins;
- track accepted-connection, peer-session, frame-handler, and disconnect-callback
  goroutines before starting them;
- stop new reconnect scheduling once draining begins;
- close the listener before waiting for tracked work;
- allow cooperative handlers and serialized frame writers to finish until the
  caller context expires; and
- force-close remaining peers at expiry, then wait until tracked goroutines have
  exited before returning.

Repeated `Drain` and `Close` calls are safe. The first transition owns teardown;
later calls wait for the same terminal state and return the same recorded close
result. A deadline or cancellation force-closes remaining sockets and is
observable as a bounded drain expiry, not as an unbounded wait.

## Shutdown participants

| Participant | Current stop trigger | Drain requirement |
| --- | --- | --- |
| Listener and accept loop | Listener close | Quiesce admissions first, then close and join |
| Inbound TLS/auth handshake | Local connection close or handshake context | Register the goroutine and reject post-drain registration |
| Framed peer reader | `shutdownCtx`, read error, or peer close | Cancel cooperatively, then force-close at deadline |
| `FrameHandler` | Handler context, handler error, or peer close | Count it as in flight and require a bounded exit |
| `TCPPeer.WriteFrame` | Write completion or connection close | Prevent new writes during drain and bound an in-progress write |
| One-shot ACK waiter | Application context, ACK, timeout, or retry budget | Application cancellation wins; transport never waits on remote delivery |
| Inventory exchange | Application context or disconnect cleanup | Stop scheduling and clear cursor entries |
| Repair continuation | Application context or disconnect cleanup | Stop scheduling, drop queued keys, and return gauges to zero |
| Static-peer reconnect | Application context and backoff | Do not schedule or redial after transport enters `Draining` |
| `OnPeerDisconnected` | Peer reader exit | Invoke once per registered peer without creating post-drain work |

## Observability

The implementation should add aggregate transport lifecycle values without peer,
address, blob, or certificate labels:

- a current shutdown-state gauge (`open`, `draining`, or `closed` represented by
  a documented numeric value);
- a shutdown-start counter;
- a drained-peer counter;
- a forced-close counter;
- a drain-deadline-expired counter; and
- gauges for tracked peer sessions and tracked lifecycle goroutines.

Structured logs should record the transition, configured deadline, peers at the
start of drain, peers forced closed, and the final outcome. Logs must not include
blob keys or introduce a high-cardinality metric label. Existing replication
metrics continue to describe canceled ACK and repair work rather than being
duplicated by transport labels.

## Alternatives considered

### Immediate close

This is the current hard-stop behavior. It is simple and remains available through
`Close`, but it does not provide a bounded opportunity for local frame handlers or
writers to finish and does not close the admission race by itself.

### Protocol shutdown message

Rejected for v0.12. A new message would require mixed-version semantics, delivery
ordering rules, and another handler path while a TCP close already communicates
the only fact peers need: this session ended. Recovery already comes from
reconnect and anti-entropy.

### Unbounded graceful wait

Rejected. A peer or handler can be slow, malicious, or permanently blocked. Every
graceful path needs a caller-owned deadline and a force-close fallback.

## Compatibility and non-goals

- Existing SHV1 framing, auth handshakes, replication messages, and peer metadata
  remain unchanged.
- Older peers observe ordinary EOF/connection close and continue to work.
- `Transport` remains source-compatible with existing implementations.
- No peer discovery, delivery guarantee, distributed transaction, or remote drain
  acknowledgment is introduced.
- This note does not change shutdown behavior, add metrics, or add a CLI flag by
  itself. Those belong in the implementation issue after the contract is reviewed.

## Verification plan

The implementation issue should add focused real-TCP coverage for:

1. a handler that finishes during the drain window;
2. a handler or write that exceeds the deadline and is force-closed;
3. inbound authentication and outbound `Dial` racing with the draining transition;
4. repeated and concurrent `Drain`/`Close` calls with one listener close and one
   disconnect callback per registered peer;
5. a delayed one-shot ACK, pending repair continuation, and reconnect backoff after
   application cancellation; and
6. zero active peers and zero tracked goroutines after return, under `-race` and a
   leak check suitable for the repository's dependency policy.

The existing full matrix, TLS/mTLS, replication, fuzz, demo, vulnerability, and
SBOM checks remain required for every implementation slice.
