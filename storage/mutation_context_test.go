package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cancelAfterChecksContext struct {
	context.Context
	nilChecks int32
	checks    atomic.Int32
	ready     chan struct{}
	readyOnce sync.Once
}

func newCancelAfterChecksContext(ctx context.Context, nilChecks int) *cancelAfterChecksContext {
	return &cancelAfterChecksContext{
		Context:   ctx,
		nilChecks: int32(nilChecks),
		ready:     make(chan struct{}),
	}
}

func (c *cancelAfterChecksContext) Err() error {
	check := c.checks.Add(1)
	if check <= c.nilChecks {
		if check == c.nilChecks {
			c.readyOnce.Do(func() { close(c.ready) })
		}
		return nil
	}
	return c.Context.Err()
}

func waitForMutationCheck(t *testing.T, ctx *cancelAfterChecksContext) {
	t.Helper()
	select {
	case <-ctx.ready:
	case <-time.After(time.Second):
		t.Fatal("mutation did not reach the pre-commit context check")
	}
}

func TestMemoryStore_PutCancellationAfterPreCommitCheck(t *testing.T) {
	store := NewMemoryStore()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newCancelAfterChecksContext(base, 1)

	store.mu.Lock()
	errCh := make(chan error, 1)
	go func() { errCh <- store.Put(ctx, []byte("key"), []byte("value")) }()
	waitForMutationCheck(t, ctx)
	cancel()
	store.mu.Unlock()

	assert.ErrorIs(t, <-errCh, context.Canceled)
	_, err := store.Get(context.Background(), []byte("key"))
	assert.ErrorIs(t, err, ErrNotFound)
	keys, err := store.ListKeys(context.Background())
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestMemoryStore_DeleteCancellationAfterPreCommitCheck(t *testing.T) {
	store := NewMemoryStore()
	key := []byte("key")
	require.NoError(t, store.Put(context.Background(), key, []byte("value")))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newCancelAfterChecksContext(base, 1)

	store.mu.Lock()
	errCh := make(chan error, 1)
	go func() { errCh <- store.Delete(ctx, key) }()
	waitForMutationCheck(t, ctx)
	cancel()
	store.mu.Unlock()

	assert.ErrorIs(t, <-errCh, context.Canceled)
	data, err := store.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
}

func TestFileStore_PutCancellationAfterPreCommitCheck(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newCancelAfterChecksContext(base, 2)

	store.mu.Lock()
	errCh := make(chan error, 1)
	go func() { errCh <- store.Put(ctx, []byte("key"), []byte("value")) }()
	waitForMutationCheck(t, ctx)
	cancel()
	store.mu.Unlock()

	assert.ErrorIs(t, <-errCh, context.Canceled)
	_, err = store.Get(context.Background(), []byte("key"))
	assert.ErrorIs(t, err, ErrNotFound)
	keys, err := store.ListKeys(context.Background())
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestFileStore_DeleteCancellationAfterPreCommitCheck(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	key := []byte("key")
	require.NoError(t, store.Put(context.Background(), key, []byte("value")))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newCancelAfterChecksContext(base, 1)

	store.mu.Lock()
	errCh := make(chan error, 1)
	go func() { errCh <- store.Delete(ctx, key) }()
	waitForMutationCheck(t, ctx)
	cancel()
	store.mu.Unlock()

	assert.ErrorIs(t, <-errCh, context.Canceled)
	data, err := store.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
}
