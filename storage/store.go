package storage

import "context"

// BlobStore is a minimal content-addressed blob API. Keys are typically hashes or opaque IDs.
type BlobStore interface {
	Put(ctx context.Context, key []byte, data []byte) error
	Get(ctx context.Context, key []byte) ([]byte, error)
	Has(ctx context.Context, key []byte) (bool, error)
	Delete(ctx context.Context, key []byte) error
}
