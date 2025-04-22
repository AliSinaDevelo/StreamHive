package storage

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrNotFound is returned when a key is missing.
	ErrNotFound = errors.New("storage: not found")
	// ErrKeyEmpty is returned for empty keys.
	ErrKeyEmpty = errors.New("storage: empty key")
)

// MemoryStore is an in-process BlobStore for tests and single-node demos.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string][]byte)}
}

func keyString(key []byte) (string, error) {
	if len(key) == 0 {
		return "", ErrKeyEmpty
	}
	return string(key), nil
}

// Put stores data under key, replacing any existing value.
func (m *MemoryStore) Put(ctx context.Context, key []byte, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ks, err := keyString(key)
	if err != nil {
		return err
	}
	cp := append([]byte(nil), data...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[ks] = cp
	return nil
}

// Get returns a copy of the value for key.
func (m *MemoryStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ks, err := keyString(key)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[ks]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Has reports whether key exists.
func (m *MemoryStore) Has(ctx context.Context, key []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ks, err := keyString(key)
	if err != nil {
		return false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[ks]
	return ok, nil
}

// Delete removes key. Missing keys are not an error.
func (m *MemoryStore) Delete(ctx context.Context, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ks, err := keyString(key)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, ks)
	return nil
}

// Len returns the number of stored blobs (for metrics/tests).
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Snapshot returns a shallow copy of keys for tests (order not stable).
func (m *MemoryStore) Snapshot() map[string][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]byte, len(m.data))
	for k, v := range m.data {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
