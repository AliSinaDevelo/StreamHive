package storage

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/google/btree"
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
	keys *btree.BTreeG[string]
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		data: make(map[string][]byte),
		keys: btree.NewOrderedG[string](32),
	}
}

func (m *MemoryStore) ensureKeyIndexLocked() {
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	if m.keys != nil {
		return
	}
	m.keys = btree.NewOrderedG[string](32)
	for key := range m.data {
		m.keys.ReplaceOrInsert(key)
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	m.ensureKeyIndexLocked()
	if _, exists := m.data[ks]; !exists {
		m.keys.ReplaceOrInsert(ks)
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := m.data[ks]; ok {
		delete(m.data, ks)
		if m.keys != nil {
			m.keys.Delete(ks)
		}
	}
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

// ListKeys returns all known keys in deterministic bytewise order.
func (m *MemoryStore) ListKeys(ctx context.Context) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.keys != nil {
		keys := make([][]byte, 0, m.keys.Len())
		var ctxErr error
		m.keys.Ascend(func(key string) bool {
			if err := ctx.Err(); err != nil {
				ctxErr = err
				return false
			}
			keys = append(keys, []byte(key))
			return true
		})
		if ctxErr != nil {
			return nil, ctxErr
		}
		return keys, nil
	}
	keys := make([][]byte, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, []byte(k))
	}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})
	return keys, nil
}

var _ BlobKeyLister = (*MemoryStore)(nil)
var _ BlobKeyPager = (*MemoryStore)(nil)

// ListKeyPage returns the smallest keys strictly after after, bounded by limit.
func (m *MemoryStore) ListKeyPage(ctx context.Context, after []byte, limit int) ([][]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	limit = normalizeKeyPageLimit(limit)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.keys != nil {
		capacity := limit
		if capacity > m.keys.Len() {
			capacity = m.keys.Len()
		}
		page := make([][]byte, 0, capacity)
		var ctxErr error
		pivot := string(after)
		m.keys.AscendGreaterOrEqual(pivot, func(key string) bool {
			if err := ctx.Err(); err != nil {
				ctxErr = err
				return false
			}
			if len(after) > 0 && key == pivot {
				return true
			}
			page = append(page, []byte(key))
			return len(page) < limit
		})
		if ctxErr != nil {
			return nil, nil, ctxErr
		}
		if len(page) == 0 {
			return nil, nil, nil
		}
		next := append([]byte(nil), page[len(page)-1]...)
		return page, next, nil
	}

	var page [][]byte
	for key := range m.data {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		page = insertKeyPage(page, []byte(key), after, limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(page) == 0 {
		return nil, nil, nil
	}
	next := append([]byte(nil), page[len(page)-1]...)
	return page, next, nil
}
