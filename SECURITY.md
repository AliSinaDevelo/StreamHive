# Security

## Reporting

If you believe you have found a security vulnerability, please open a **private** advisory on GitHub for this repository or contact the maintainers with details and reproduction steps. Do not file public issues for undisclosed vulnerabilities.

## Scope

This project is experimental research code. It is not hardened for hostile networks; treat deployments accordingly.

## Transport Security And Identity

StreamHive can wrap peer connections in TLS:

- `-tls-cert` and `-tls-key` enable TLS on the listener.
- `-tls-ca` and `-tls-server-name` enable outbound certificate verification.
- `-tls-insecure-skip-verify` is for local development only.

Library users can configure `p2p.TCPTransport.TLSServerConfig` and
`p2p.TCPTransport.TLSClientConfig` directly. Use `tls.Config.ClientAuth`,
`ClientCAs`, and client certificates when you need mTLS.

TLS protects the TCP channel and, when configured with CA verification, authenticates the
certificate presented by the peer.

StreamHive also has optional shared-token peer admission:

- `-peer-auth-token` requires peers to send a matching token before registration.
- `-peer-auth-timeout` bounds how long a connection can sit in the auth handshake.
- `-peer-id` exchanges a bounded printable application identity for peer snapshots and logs.
- `-peer-allow-ids` applies an exact inbound identity allowlist after token validation.
- Library users can set `p2p.TCPTransport.PeerAuthToken` and `PeerAuthTimeout`.
- Library users can set `PeerAuthIdentity` and `PeerAuthAllowedIdentities` for the same identity boundary.

The exchanged identity is an operational label, not a cryptographic proof. The allowlist
provides exact inbound authorization scoped to the shared token, but it is not a full ACL
system and replication messages are not signed. Use shared-token auth with TLS/mTLS when
the token or identity crosses a network boundary, and rotate the token if it appears in
logs, shell history, or process metadata. Do not expose the P2P port to untrusted networks
without a deployment-level trust boundary.

## Dependency scanning

CI runs `govulncheck ./...` on each push and pull request to `main`.

## SBOM

The `sbom` CI job emits a CycloneDX JSON bill of materials (`sbom.cdx.json`) as a workflow artifact for supply-chain review.
