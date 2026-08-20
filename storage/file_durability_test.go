package storage

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStore_PutReportsPostRenameSyncFailureWithoutRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	_, err = store.ListKeys(ctx)
	require.NoError(t, err)

	syncErr := errors.New("directory sync failed")
	store.syncDir = func(string) error { return syncErr }

	key := []byte("put-sync-failure")
	err = store.Put(ctx, key, []byte("value"))
	require.ErrorIs(t, err, syncErr)

	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), got)
	keys, err := store.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{key}, keys)
	assertNoTemporaryFiles(t, dir)
}

func TestFileStore_DeleteReportsPostRemoveSyncFailureWithoutRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	key := []byte("delete-sync-failure")
	require.NoError(t, store.Put(ctx, key, []byte("value")))
	_, err = store.ListKeys(ctx)
	require.NoError(t, err)

	syncErr := errors.New("directory sync failed")
	store.syncDir = func(string) error { return syncErr }

	err = store.Delete(ctx, key)
	require.ErrorIs(t, err, syncErr)

	_, err = store.Get(ctx, key)
	assert.ErrorIs(t, err, ErrNotFound)
	keys, err := store.ListKeys(ctx)
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func assertNoTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".streamhive-")
	}
}
