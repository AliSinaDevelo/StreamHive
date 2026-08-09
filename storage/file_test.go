package storage

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStore_PutGetRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	key := []byte("alpha")
	require.NoError(t, store.Put(ctx, key, []byte("hello")))

	reopened, err := NewFileStore(dir)
	require.NoError(t, err)
	got, err := reopened.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got)
}

func TestFileStore_HexEncodesKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	key := []byte("../escape")
	require.NoError(t, store.Put(ctx, key, []byte("safe")))

	encoded := hex.EncodeToString(key)
	data, err := os.ReadFile(filepath.Join(dir, encoded))
	require.NoError(t, err)
	assert.Equal(t, []byte("safe"), data)
	assert.NoFileExists(t, filepath.Join(dir, "..", "escape"))
}

func TestFileStore_PutReplace(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	key := []byte("k")
	require.NoError(t, store.Put(ctx, key, []byte("a")))
	require.NoError(t, store.Put(ctx, key, []byte("b")))

	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), got)
}

func TestFileStore_NotFoundAndHas(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	has, err := store.Has(ctx, []byte("missing"))
	require.NoError(t, err)
	assert.False(t, has)

	_, err = store.Get(ctx, []byte("missing"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFileStore_ContentKeyDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	data := []byte("durable content")
	key := SHA256Key(data)
	require.NoError(t, store.Put(ctx, key, data))
	require.NoError(t, os.WriteFile(filepath.Join(dir, SHA256KeyHex(data)), []byte("tampered"), 0o600))

	has, err := store.Has(ctx, key)
	require.NoError(t, err)
	assert.False(t, has)
	_, err = store.Get(ctx, key)
	assert.ErrorIs(t, err, ErrSHA256Mismatch)
}

func TestFileStore_Delete(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	key := []byte("k")
	require.NoError(t, store.Put(ctx, key, []byte("value")))
	require.NoError(t, store.Delete(ctx, key))
	require.NoError(t, store.Delete(ctx, key))

	has, err := store.Has(ctx, key)
	require.NoError(t, err)
	assert.False(t, has)
}

func TestFileStore_ListKeysRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.Put(ctx, []byte("b"), []byte("2")))
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("1")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".streamhive-temp"), []byte("tmp"), 0o600))

	reopened, err := NewFileStore(dir)
	require.NoError(t, err)
	keys, err := reopened.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, keys)

	keys[0][0] = 'x'
	again, err := reopened.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, again)
}

func TestFileStore_ListKeysRejectsMalformedRegularFilename(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "not-a-hex-key"), []byte("payload"), 0o600))

	_, err = store.ListKeys(ctx)
	assert.ErrorIs(t, err, ErrInvalidKeyFilename)

	_, _, err = store.ListKeyPage(ctx, nil, 1)
	assert.ErrorIs(t, err, ErrInvalidKeyFilename)
}

func TestFileStore_SkipsNonRegularEntriesAndRejectsDirectReads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	regularKey := []byte("regular")
	directoryKey := []byte("directory")
	symlinkKey := []byte("symlink")
	require.NoError(t, store.Put(ctx, regularKey, []byte("payload")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, hex.EncodeToString(directoryKey)), 0o700))
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "outside")
	require.NoError(t, os.WriteFile(targetPath, []byte("outside payload"), 0o600))
	require.NoError(t, os.Symlink(targetPath, filepath.Join(dir, hex.EncodeToString(symlinkKey))))
	require.NoError(t, os.Symlink(targetPath, filepath.Join(dir, "not-a-hex-link")))

	reopened, err := NewFileStore(dir)
	require.NoError(t, err)
	keys, err := reopened.ListKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{regularKey}, keys)

	has, err := reopened.Has(ctx, directoryKey)
	require.NoError(t, err)
	assert.False(t, has)
	_, err = reopened.Get(ctx, directoryKey)
	assert.ErrorIs(t, err, ErrNonRegularEntry)

	has, err = reopened.Has(ctx, symlinkKey)
	require.NoError(t, err)
	assert.False(t, has)
	_, err = reopened.Get(ctx, symlinkKey)
	assert.ErrorIs(t, err, ErrNonRegularEntry)
}

func TestFileStore_EmptyKey(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	assert.ErrorIs(t, store.Put(ctx, nil, []byte("value")), ErrKeyEmpty)
	assert.ErrorIs(t, store.Delete(ctx, nil), ErrKeyEmpty)
}

func TestFileStore_ContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	assert.Error(t, store.Put(ctx, []byte("k"), []byte("value")))
	_, err = store.Get(ctx, []byte("k"))
	assert.Error(t, err)
}

func TestFileStore_ListKeysContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.ListKeys(ctx)
	assert.Error(t, err)
}

func TestFileStore_ListKeyPageRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	for _, key := range []string{"d", "b", "a", "c"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("value")))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".streamhive-temp"), []byte("tmp"), 0o600))

	reopened, err := NewFileStore(dir)
	require.NoError(t, err)
	page, next, err := reopened.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, page)
	assert.Equal(t, []byte("b"), next)

	page, next, err = reopened.ListKeyPage(ctx, next, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("c"), []byte("d")}, page)
	assert.Equal(t, []byte("d"), next)

	page, next, err = reopened.ListKeyPage(ctx, next, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestFileStore_ListKeyPageContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	_, _, err = store.ListKeyPage(ctx, nil, 2)
	assert.Error(t, err)
}

func TestFileStore_ListKeyPageEmpty(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	page, next, err := store.ListKeyPage(context.Background(), nil, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestFileStore_ListKeyPageReflectsMutations(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	for _, key := range []string{"a", "c", "d"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("value")))
	}

	page, next, err := store.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("c")}, page)
	assert.Equal(t, []byte("c"), next)

	require.NoError(t, store.Delete(ctx, []byte("d")))
	require.NoError(t, store.Put(ctx, []byte("b"), []byte("value")))
	cursor := next
	page, next, err = store.ListKeyPage(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)

	require.NoError(t, store.Put(ctx, []byte("e"), []byte("value")))
	page, next, err = store.ListKeyPage(ctx, cursor, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("e")}, page)
	assert.Equal(t, []byte("e"), next)
}

func TestFileStore_ListKeyPageRefreshesExternalMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)
	external, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("value")))

	_, _, err = store.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	require.NoError(t, external.Put(ctx, []byte("b"), []byte("value")))

	page, next, err := store.ListKeyPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("a"), []byte("b")}, page)
	assert.Equal(t, []byte("b"), next)
}

func TestNewFileStore_EmptyDirectory(t *testing.T) {
	_, err := NewFileStore("")
	require.Error(t, err)
}
