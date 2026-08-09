# Storage Integrity

## Endpoint

When the node runs with `-replicate`, `GET /storage/status` performs a live read of the local
blob inventory. It returns aggregate JSON without blob keys, raw content, peer identities, or raw
storage errors:

```json
{
  "enabled": true,
  "healthy": false,
  "scan_consistency": "live",
  "keys": 3,
  "key_bytes": 83,
  "content_addressed_keys": 2,
  "verified_keys": 1,
  "verified_bytes": 24,
  "opaque_keys": 1,
  "opaque_bytes": 14,
  "corrupt_keys": 1,
  "missing_keys": 0
}
```

`healthy` is true only when the scan finds no corrupt or missing entry. A raw-only node returns
`enabled: false`, `healthy: true`, `scan_consistency: "live"`, and zero counts. A configured-store
enumeration or read failure returns HTTP `503` with the generic body `storage unavailable`.

## Classification

- Every listed key contributes to `keys` and `key_bytes`.
- A 32-byte key is a SHA-256 content address. Its current bytes must hash to the key to count as
  `verified_keys` and `verified_bytes`; a mismatch counts as `corrupt_keys`.
- A non-32-byte key is opaque. Present bytes contribute to `opaque_bytes`, while the key remains in
  `opaque_keys` even if its read is missing.
- `missing_keys` counts listed keys whose current value cannot be read because it is absent.

The scan does not rewrite, delete, repair, or quarantine any blob. Corruption and missing entries
are observations for an operator or later anti-entropy pass.

## Scan Semantics

Native `BlobKeyPager` stores are read one bounded page at a time. Older `BlobKeyLister` stores use
the complete-list compatibility path. Store mutations between page calls can be observed, so
`scan_consistency: "live"` is not a snapshot guarantee. Request cancellation is checked between
pages and blob reads.

The endpoint does not alter `/readyz`: a node can remain ready while an operator investigates a
corrupt or incomplete local inventory. It also does not add a replication message, tombstone,
revision, deletion, or peer-discovery behavior.

## Metrics

`/metrics` and `/metrics/prometheus` expose the label-free
`replication_storage_integrity_*` families for scan starts, completions, failures, scanned keys and
key bytes, content-addressed keys, verified keys and bytes, opaque keys and bytes, corrupt keys,
missing keys, and cumulative scan duration in milliseconds. Counts and bytes accumulate for
successful scans; failed scans increment only the attempt, failure, and duration families.

Run the focused proof with:

```bash
make test-storage-integrity
```
