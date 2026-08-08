package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyInventoryLister struct {
	keys [][]byte
}

func (l legacyInventoryLister) ListKeys(ctx context.Context) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keys := make([][]byte, len(l.keys))
	for i, key := range l.keys {
		keys[i] = append([]byte(nil), key...)
	}
	return keys, nil
}

type boundedInventoryPager struct {
	store *MemoryStore
	calls int
}

func (p *boundedInventoryPager) ListKeys(ctx context.Context) ([][]byte, error) {
	return p.store.ListKeys(ctx)
}

func (p *boundedInventoryPager) ListKeyPage(ctx context.Context, after []byte, limit int) ([][]byte, []byte, error) {
	p.calls++
	if limit > 2 {
		limit = 2
	}
	return p.store.ListKeyPage(ctx, after, limit)
}

func expectedInventoryDigest(keys [][]byte) [sha256.Size]byte {
	sorted := make([][]byte, len(keys))
	for i, key := range keys {
		sorted[i] = append([]byte(nil), key...)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i], sorted[j]) < 0
	})
	h := sha256.New()
	for _, key := range sorted {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(key)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(key)
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func TestSummarizeInventoryUsesBoundedPager(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	keys := [][]byte{
		{0xff, 0x00},
		{0x00, 0x01, 0x02},
		[]byte("middle"),
		{0x10},
		[]byte("last"),
	}
	for _, key := range keys {
		require.NoError(t, store.Put(ctx, key, []byte("value")))
	}
	pager := &boundedInventoryPager{store: store}

	summary, err := SummarizeInventory(ctx, pager)

	require.NoError(t, err)
	assert.Equal(t, len(keys), summary.KeyCount)
	assert.Equal(t, int64(2+3+6+1+4), summary.KeyBytes)
	assert.Equal(t, expectedInventoryDigest(keys), summary.Digest)
	assert.Greater(t, pager.calls, 1)
}

func TestSummarizeInventorySortsLegacyLister(t *testing.T) {
	keys := [][]byte{[]byte("z"), []byte("a-longer-key"), []byte("middle")}
	summary, err := SummarizeInventory(context.Background(), legacyInventoryLister{keys: keys})

	require.NoError(t, err)
	assert.Equal(t, len(keys), summary.KeyCount)
	assert.Equal(t, int64(1+len("a-longer-key")+len("middle")), summary.KeyBytes)
	assert.Equal(t, expectedInventoryDigest(keys), summary.Digest)
}

func TestSummarizeInventoryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := SummarizeInventory(ctx, legacyInventoryLister{keys: [][]byte{[]byte("key")}})

	assert.ErrorIs(t, err, context.Canceled)
}

func TestSummarizeInventoryRejectsInvalidPager(t *testing.T) {
	invalid := invalidInventoryPager{}

	_, err := SummarizeInventory(context.Background(), invalid)

	assert.ErrorIs(t, err, ErrInventoryPagerInvalid)
}

type invalidInventoryPager struct{}

func (invalidInventoryPager) ListKeys(context.Context) ([][]byte, error) {
	return nil, nil
}

func (invalidInventoryPager) ListKeyPage(context.Context, []byte, int) ([][]byte, []byte, error) {
	return [][]byte{[]byte("key")}, []byte("wrong-cursor"), nil
}
