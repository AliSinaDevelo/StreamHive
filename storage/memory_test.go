package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStore_PutGet(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := []byte("k1")
	val := []byte("hello")
	require.NoError(t, s.Put(ctx, key, val))
	got, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, val, got)

	has, err := s.Has(ctx, key)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestMemoryStore_NotFound(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_, err := s.Get(ctx, []byte("missing"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := []byte("k")
	require.NoError(t, s.Put(ctx, key, []byte("x")))
	require.NoError(t, s.Delete(ctx, key))
	_, err := s.Get(ctx, key)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryStore_EmptyKey(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	assert.ErrorIs(t, s.Put(ctx, nil, []byte("x")), ErrKeyEmpty)
}

func TestMemoryStore_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewMemoryStore()
	assert.Error(t, s.Put(ctx, []byte("k"), []byte("v")))
}

func TestMemoryStore_PutReplace(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := []byte("k")
	require.NoError(t, s.Put(ctx, key, []byte("a")))
	require.NoError(t, s.Put(ctx, key, []byte("b")))
	got, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), got)
}

func TestMemoryStore_GetCopy(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	key := []byte("k")
	require.NoError(t, s.Put(ctx, key, []byte("x")))
	a, err := s.Get(ctx, key)
	require.NoError(t, err)
	b, err := s.Get(ctx, key)
	require.NoError(t, err)
	a[0] = 'y'
	assert.Equal(t, byte('x'), b[0])
}

func TestMemoryStore_Len(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	assert.Equal(t, 0, s.Len())
	require.NoError(t, s.Put(ctx, []byte("a"), []byte("1")))
	assert.Equal(t, 1, s.Len())
}

func TestMemoryStore_ListKeys(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	require.NoError(t, s.Put(ctx, []byte("b"), []byte("2")))
	require.NoError(t, s.Put(ctx, []byte("a"), []byte("1")))

	keys, err := s.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, keys)

	keys[0][0] = 'x'
	again, err := s.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, again)
}

func TestMemoryStore_ContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := NewMemoryStore()
	_, err := s.Get(ctx, []byte("k"))
	assert.Error(t, err)
}

func TestMemoryStore_ListKeysContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := NewMemoryStore()
	_, err := s.ListKeys(ctx)
	assert.Error(t, err)
}

func TestMemoryStore_ListKeyPage(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	for _, key := range []string{"d", "b", "a", "c"} {
		require.NoError(t, s.Put(ctx, []byte(key), []byte("value")))
	}

	page, next, err := s.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, page)
	assert.Equal(t, []byte("b"), next)

	page, next, err = s.ListKeyPage(ctx, next, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("c"), []byte("d")}, page)
	assert.Equal(t, []byte("d"), next)

	page, next, err = s.ListKeyPage(ctx, next, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestMemoryStore_ListKeyPageUsesDefaultLimit(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	for i := 0; i < DefaultKeyPageSize+1; i++ {
		require.NoError(t, s.Put(ctx, []byte{byte(i >> 8), byte(i)}, []byte("value")))
	}

	page, next, err := s.ListKeyPage(ctx, nil, 0)
	require.NoError(t, err)
	assert.Len(t, page, DefaultKeyPageSize)
	assert.Equal(t, page[len(page)-1], next)
}

func TestMemoryStore_ListKeyPageContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := NewMemoryStore()

	_, _, err := s.ListKeyPage(ctx, nil, 2)
	assert.Error(t, err)
}

func TestMemoryStore_ListKeyPageEmpty(t *testing.T) {
	page, next, err := NewMemoryStore().ListKeyPage(context.Background(), nil, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestMemoryStore_ListKeyPageReflectsMutations(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	for _, key := range []string{"a", "c", "d"} {
		require.NoError(t, s.Put(ctx, []byte(key), []byte("value")))
	}

	page, next, err := s.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("c")}, page)
	assert.Equal(t, []byte("c"), next)

	require.NoError(t, s.Delete(ctx, []byte("d")))
	require.NoError(t, s.Put(ctx, []byte("b"), []byte("value")))
	cursor := next
	page, next, err = s.ListKeyPage(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)

	require.NoError(t, s.Put(ctx, []byte("e"), []byte("value")))
	page, next, err = s.ListKeyPage(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("e")}, page)
	assert.Equal(t, []byte("e"), next)
}
