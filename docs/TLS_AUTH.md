# TLS And Application Auth

Status: v0.12 acceptance contract, tracked by issue #37.

## Contract

StreamHive has two distinct admission layers:

1. TLS protects the TCP channel and verifies the listener certificate for outbound CLI peers.
2. The shared-token handshake authenticates the StreamHive application peer.
3. `-peer-allow-ids` applies exact inbound authorization to the bounded application identity.
4. Only after those checks does the transport register the peer and apply `-max-peers`.

The positive CLI path uses `-tls-ca` and `-tls-server-name`. It does not use
`-tls-insecure-skip-verify`. A failed CA or hostname check fails the dial before the peer is
registered, so replication frames do not reach the application handler.

Certificate subjects and serial numbers are not emitted as metric labels. `/peers` exposes the
application `auth_method` and `auth_identity`; `/metrics` and `/metrics/prometheus` expose
aggregate TLS-adjacent admission outcomes through the existing transport and replication
counters.

## CLI Workflow

The listener loads its certificate and private key:

```bash
go run . \
  -listen 0.0.0.0:7070 \
  -health 0.0.0.0:8080 \
  -replicate -store-dir ./streamhive-data \
  -tls-cert ./server-cert.pem -tls-key ./server-key.pem \
  -peer-auth-token "$STREAMHIVE_PEER_TOKEN" \
  -peer-id server -peer-allow-ids client
```

The outbound peer supplies the trusted CA and the DNS name present in the certificate:

```bash
go run . \
  -listen 127.0.0.1:0 -dial 127.0.0.1:7070 \
  -replicate \
  -tls-ca ./ca.pem -tls-server-name streamhive.example \
  -peer-auth-token "$STREAMHIVE_PEER_TOKEN" \
  -peer-id client
```

The certificate must be valid for `streamhive.example`, and the CA file must contain its trust
chain. The listener checks the client identity against `client` after token validation. An empty
allowlist keeps token-only compatibility; an exact allowlist is the stronger application policy.

Use `-tls-insecure-skip-verify` only for local development experiments. It disables normal
certificate verification and is not part of the acceptance path.

## Library mTLS Boundary

The CLI currently exposes server certificates and outbound server verification. Library users who
need mutual certificate authentication configure `p2p.TCPTransport.TLSServerConfig` and
`TLSClientConfig` directly, including `tls.Config.ClientAuth`, `ClientCAs`, and client
certificates. The transport completes an inbound TLS handshake before application auth, peer-cap
admission, `OnPeer`, or frame handling. `TLSHandshakeTimeout` bounds that work and defaults to
`DefaultTLSHandshakeTimeout` when unset. The application token and identity allowlist remain useful
as a separate policy layer; neither one is a substitute for certificate verification, and
replication messages are not individually signed.

`tls_handshake_success` and `tls_handshake_failures` are aggregate local transport counters with
no certificate or address labels. An outbound `Dial` reports its local TLS result; mTLS has no
remote-admission acknowledgment at the transport API, so a server-side certificate rejection is
observed by the outbound peer as a connection close and `OnPeerDisconnected` callback.

## Acceptance Evidence

Run the focused proof with:

```bash
make test-tls-auth
```

The target repeats the following under the race detector:

- `TestRun_tlsPeerAuthReplicatesContentBlob` generates an ephemeral CA/server certificate, verifies
  the configured CA and hostname, admits the exact application identity, replicates a SHA-256 blob,
  and checks `/peers` plus JSON/Prometheus metrics.
- `TestRun_tlsVerificationRejectsBeforePeerAdmission` checks wrong-CA and wrong-server-name paths;
  both fail before application auth succeeds or a blob is stored.

The library target is:

```bash
make test-mtls
```

`TestTCPTransport_mutualTLSAdmitsVerifiedClient` configures `ClientAuth:
tls.RequireAndVerifyClientCert`, exchanges a frame, and checks the handshake metrics. The rejection
test covers a missing and an unrelated client certificate; the server records a TLS failure without
calling `OnPeer` or the frame handler, and the client observes the resulting disconnect.

Certificate lifecycle and restart-only rotation are defined in
[TLS_ROTATION.md](TLS_ROTATION.md). Replacing files on disk does not change active connections;
planned rotation requires preflight validation, process restart, and bounded static-peer
reconnect.

## Research Sources

- [Go `crypto/tls` package](https://pkg.go.dev/crypto/tls) documents `RootCAs`, `ServerName`,
  `HandshakeContext`, and the testing-only risk of `InsecureSkipVerify`.
- [Go TLS verification example](https://go.dev/src/crypto/tls/example_test.go) shows custom
  certificate verification and the server-side client-certificate boundary.
- [RFC 8446](https://datatracker.ietf.org/doc/html/rfc8446) defines TLS 1.3 certificate
  authentication and the `CertificateVerify` proof of private-key possession.
