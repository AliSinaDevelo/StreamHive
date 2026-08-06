# TLS Certificate Rotation

Status: v0.12 contract, researched in issue #39 and proven by issue #40.

This document defines how StreamHive handles TLS and mTLS credential changes. It is deliberately
about lifecycle behavior, not certificate issuance. The current contract is restart-first: a
process reads its credential material during startup, validates it before binding the listener,
and uses that immutable configuration for the lifetime of the process.

## Current lifecycle

### CLI

- `-tls-cert` and `-tls-key` are loaded once with `tls.LoadX509KeyPair` before
  `TCPTransport.ListenAndAccept` binds the listener.
- `-tls-ca` is read once and parsed into the outbound `tls.Config` before the first dial.
- The listener is a `tls.NewListener` around one TCP listener. Each accepted connection performs
  a bounded TLS handshake before application token auth, peer registration, `OnPeer`, or frame
  handling.
- Each outbound `Dial` constructs a TLS client from the configured client config and performs a
  bounded handshake before application auth and peer registration.
- Existing peers own established `*tls.Conn` values. Replacing certificate files on disk does
  not change those connections or cause a renegotiation.
- `-peer-reconnect` retries only static `-peers` targets with bounded exponential backoff. A
  reconnect creates a new TCP and TLS connection, so it observes the credentials configured by
  the process at that time.
- shutdown closes the listener and active peers. A restarted process must load and validate its
  credentials again before it can accept connections.

### Library

Library users provide `p2p.TCPTransport.TLSServerConfig` and `TLSClientConfig`. The transport
does not mutate or replace those configs and has no TLS reload or transport restart API. The Go
TLS package requires a `tls.Config` not be modified after it has been passed to a TLS function;
callers that need a new immutable configuration must construct it separately or use `Config.Clone`
as the starting point.

## Rotation options

| Option | Existing peers | New handshakes | Failed replacement | v0.12 decision |
| --- | --- | --- | --- | --- |
| Restart-only | Continue until the process is stopped; a planned restart closes them | Read the newly staged files on process start | Old process stays intact when validation happens before shutdown; supervisor policy decides whether to retry | **Selected** |
| Callback-backed certificate selection | Unchanged | `GetCertificate`, `GetClientCertificate`, or `GetConfigForClient` can select a snapshot per handshake | Requires an application-owned atomic snapshot and a defined fallback | Library escape hatch only; not a StreamHive CLI contract |
| Atomic config replacement | Unchanged | Could publish an immutable `*tls.Config` to future dials and callbacks | Can retain the last valid snapshot if the replacement is validated before publication | Deferred until a bounded API and metrics exist |
| Explicit transport restart | Closed and rebuilt by the library | Uses the replacement config after listener recreation | Requires port handoff, active-peer policy, and rollback behavior | Deferred; process restart is the supported operation |

The callback options are useful Go primitives, but they do not by themselves solve rotation. A
callback needs synchronization, immutable certificate objects, error handling, and an explicit
answer for session resumption and mTLS policy changes. An explicit transport restart would also
need to define whether active peers are drained, immediately closed, or reconnected, and how a
failed listener bind is reported.

## v0.12 contract

1. Stage new certificate, key, and trust material outside the running process.
2. Validate the complete replacement before stopping the current process.
3. Restart the node under its supervisor. Startup validation happens before the new listener is
   considered ready.
4. Keep static `-peers` plus `-peer-reconnect` enabled when reconnect after restart is required.
   Reconnect attempts use the process's newly loaded TLS configuration and existing bounded
   backoff settings.
5. Rotate a cluster in an order that preserves trust overlap. When a CA changes, deploy the
   overlapping trust bundle to clients before presenting certificates signed only by the new CA.
6. Treat active connections as using their original TLS session until they disconnect. StreamHive
   does not claim live certificate replacement, renegotiation, or zero-downtime rotation.

The contract is the same for library mTLS: the transport performs admission with the config it
was constructed with. A caller may build a new transport with a new immutable config, but
StreamHive does not promise that changing fields on an in-use config is safe.

### Session resumption

StreamHive does not promise TLS session resumption across a process restart. The CLI does not
configure a client session cache, and a new server process gets fresh automatically managed
session-ticket state. Library callers that supply session caches or explicit ticket keys own the
compatibility and security consequences. In particular, Go documents that resumed connections
can bypass `VerifyPeerCertificate`; use `VerifyConnection` or disable tickets when a policy must
run for every connection.

## Operator rotation runbook

1. Confirm the new server certificate has the required DNS/IP names, validity window, key usage,
   and the CA chain trusted by every peer. For mTLS, confirm the new client certificate and
   server `ClientAuth` trust policy as well.
2. Stage the new files with restrictive key permissions. Write temporary files and rename them
   into place only after the complete files are present.
3. Validate the certificate/key pair and trust bundle before taking a node down. An invalid
   replacement must fail a preflight check, not be discovered after the only healthy process is
   stopped.
4. If the trust root changes, deploy an overlapping CA bundle to outbound peers first. Keep the
   old trust path available until every server has moved to the replacement certificate.
5. Restart one node at a time. Wait for its health endpoint and `readyz`, then verify that its
   static peers reconnect before continuing. Keep the configured reconnect maximum bounded.
6. Inspect aggregate `tls_handshake_success`, `tls_handshake_failures`, `dial_errors`, and
   `active_peers` counters. Confirm a known content-addressed blob can still be read or repaired.
7. If startup validation or readiness fails, restore the previous files and restart. Do not
   overwrite the last known-good material until the replacement has been observed healthy.

## Focused verification plan

The acceptance proof for the restart-only contract must use real TCP and cover:

- an old TLS connection is admitted before rotation and is closed by the planned restart;
- the restarted listener accepts the same peer with the replacement certificate;
- a static peer reconnects with bounded backoff and reaches application admission again;
- a failed replacement is rejected before listener readiness and does not publish a partial config;
- restoring the old material permits a subsequent restart and reconnect;
- mTLS still rejects a missing or unrelated client certificate before `OnPeer` and frame handling;
- handshake success/failure counters remain aggregate and contain no certificate, secret, or
  remote-address labels.

The test must not rely on a particular certificate serial number in metrics or logs. It should
assert the externally visible outcomes: handshake, reconnect, readiness, peer admission, and
blob convergence.

The shipped real-TCP proof is:

```bash
make test-tls-rotation
```

It repeats the rotation, malformed-startup, rollback, certificate-replacement, reconnect, and
aggregate-metric checks under the race detector.

## Non-goals

- No SIGHUP, file watcher, admin endpoint, or public reload method in v0.12.
- No claim that replacing files on disk changes active connections.
- No implicit session-ticket invalidation or cross-process resumption guarantee.
- No certificate fingerprints, private material, or peer addresses as metric labels.
- No change to the existing TLS-before-application-auth ordering or the mixed-version wire
  protocol.

## References

- [Go `crypto/tls` package](https://pkg.go.dev/crypto/tls), especially `Config`, callbacks,
  `Clone`, `SessionTicketsDisabled`, and `ClientSessionCache`.
- [Go `crypto/tls` source](https://go.dev/src/crypto/tls/).
- [RFC 8446, TLS 1.3](https://datatracker.ietf.org/doc/html/rfc8446), including session
  resumption and `NewSessionTicket`.
