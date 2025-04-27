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

func TestMemoryStore_ContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := NewMemoryStore()
	_, err := s.Get(ctx, []byte("k"))
	assert.Error(t, err)
}
