package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type integrityPager struct {
	store *MemoryStore
	calls int
}

func (p *integrityPager) ListKeys(ctx context.Context) ([][]byte, error) {
	return p.store.ListKeys(ctx)
}

func (p *integrityPager) ListKeyPage(ctx context.Context, after []byte, limit int) ([][]byte, []byte, error) {
	p.calls++
	if limit > 2 {
		limit = 2
	}
	return p.store.ListKeyPage(ctx, after, limit)
}

type integrityLegacyLister struct {
	keys [][]byte
}

func (l integrityLegacyLister) ListKeys(ctx context.Context) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keys := make([][]byte, len(l.keys))
	for i, key := range l.keys {
		keys[i] = append([]byte(nil), key...)
	}
	return keys, nil
}

func TestInspectInventoryClassifiesContentAndOpaqueKeys(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	validData := []byte("verified content")
	validKey := SHA256Key(validData)
	corruptKey := SHA256Key([]byte("expected content"))
	opaqueKey := []byte("opaque-key")
	opaqueData := []byte("opaque content")
	require.NoError(t, store.Put(ctx, validKey, validData))
	require.NoError(t, store.Put(ctx, corruptKey, []byte("tampered content")))
	require.NoError(t, store.Put(ctx, opaqueKey, opaqueData))
	pager := &integrityPager{store: store}

	summary, err := InspectInventory(ctx, store, pager)

	require.NoError(t, err)
	assert.Equal(t, 3, summary.KeyCount)
	assert.Equal(t, int64(len(validKey)+len(corruptKey)+len(opaqueKey)), summary.KeyBytes)
	assert.Equal(t, 2, summary.ContentAddressedKeys)
	assert.Equal(t, 1, summary.VerifiedKeys)
	assert.Equal(t, int64(len(validData)), summary.VerifiedBytes)
	assert.Equal(t, 1, summary.OpaqueKeys)
	assert.Equal(t, int64(len(opaqueData)), summary.OpaqueBytes)
	assert.Equal(t, 1, summary.CorruptKeys)
	assert.Zero(t, summary.MissingKeys)
	assert.Greater(t, pager.calls, 1)
}

func TestInspectInventoryClassifiesMissingKeysWithLegacyLister(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	data := []byte("present")
	presentKey := SHA256Key(data)
	missingContentKey := SHA256Key([]byte("missing content"))
	missingOpaqueKey := []byte("missing-opaque")
	require.NoError(t, store.Put(ctx, presentKey, data))

	summary, err := InspectInventory(ctx, store, integrityLegacyLister{keys: [][]byte{
		missingOpaqueKey,
		missingContentKey,
		presentKey,
	}})

	require.NoError(t, err)
	assert.Equal(t, 3, summary.KeyCount)
	assert.Equal(t, 2, summary.ContentAddressedKeys)
	assert.Equal(t, 1, summary.VerifiedKeys)
	assert.Equal(t, 1, summary.OpaqueKeys)
	assert.Equal(t, 2, summary.MissingKeys)
}

func TestInspectInventoryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := InspectInventory(ctx, NewMemoryStore(), integrityLegacyLister{keys: [][]byte{[]byte("key")}})

	assert.ErrorIs(t, err, context.Canceled)
}

func TestInspectInventoryRejectsInvalidPager(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Put(context.Background(), []byte("key"), []byte("value")))

	_, err := InspectInventory(context.Background(), store, invalidInventoryPager{})

	assert.ErrorIs(t, err, ErrInventoryPagerInvalid)
}
