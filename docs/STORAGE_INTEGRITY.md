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
  "key_bytes": 88,
  "content_addressed_keys": 2,
  "verified_keys": 1,
  "verified_bytes": 26,
  "opaque_keys": 1,
  "opaque_bytes": 14,
  "corrupt_keys": 1,
  "missing_keys": 0
}
```

`healthy` is true only when the scan finds no corrupt or missing entry. A raw-only node returns
`enabled: false`, `healthy: true`, `scan_consistency: "live"`, and zero counts. A configured-store
enumeration or read failure returns HTTP `503` with the generic body `storage unavailable`.

FileStore inventory treats only regular files with hex-encoded names as blob entries. Temporary
files, directories, symlinks, and other non-regular entries are ignored. A malformed regular name
fails enumeration with `storage.ErrInvalidKeyFilename`; the HTTP endpoint keeps the generic `503`
boundary. Direct `FileStore.Get` calls return `storage.ErrNonRegularEntry` for a non-regular path,
and `Has` reports that path as absent without reading through it. Direct reads bind the opened file
to the regular entry observed by `Lstat` and consume that descriptor only when the file identities
match; an entry replacement that resolves to a different file is rejected as
`storage.ErrNonRegularEntry` rather than read through.

`FileStore.Delete` uses the same entry boundary for local eviction: a regular keyed file may be
removed, a missing key remains a successful no-op, and a directory, symlink, or other non-regular
keyed path returns `storage.ErrNonRegularEntry` without being removed. This is local filesystem
classification, not a distributed logical-delete or tombstone operation.

## Mutation Durability

`FileStore.Put` writes the blob to a private temporary file, calls `Sync` on that file before the
atomic replacement, and calls `Sync` on the store directory after the replacement. `FileStore.Delete`
syncs the store directory after removing an existing blob. A successful mutation therefore includes
the filesystem's requested file and directory durability boundary, covering recovery after an
ordinary process restart and reducing the crash/power-loss window on filesystems that honor these
operations. This is still local storage durability, not a replicated acknowledgement or a hardware
durability guarantee.

If the rename or directory sync fails after a filesystem mutation has begun, the method returns the
error without claiming rollback; callers should treat the result as uncertain and use the existing
integrity scan or an idempotent retry. Context cancellation is checked before the mutation commit,
but does not undo a rename, remove, or sync that has already started.

## Offline Verification

The CLI can run the same aggregate scan without opening a TCP or health listener:

```bash
go run . -store-dir /var/lib/streamhive/blobs -verify-store
```

`-verify-store` prints the status JSON above and exits successfully only when the scan is healthy.
Corrupt or missing entries produce the JSON result followed by a non-zero exit status; malformed
regular filenames and other enumeration or read failures fail the command before a result is
printed. The command never rewrites, deletes, repairs, or quarantines data, and its output remains
aggregate-only. It does not require `-replicate` and cannot be combined with `-list-keys`.

## Classification

- Every listed key contributes to `keys` and `key_bytes`.
- A 32-byte key is a SHA-256 content address. Its current bytes must hash to the key to count as
  `verified_keys` and `verified_bytes`; a mismatch counts as `corrupt_keys`.
- A non-32-byte key is opaque. Present bytes contribute to `opaque_bytes`, while the key remains in
  `opaque_keys` even if its read is missing.
- `missing_keys` counts listed keys whose current value cannot be read because it is absent.
- Only regular, hex-named files become listed keys. Non-regular entries are outside the inventory;
  they are not counted as missing or corrupt blobs.

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
make test-storage-verify
make test-storage-durability
```
