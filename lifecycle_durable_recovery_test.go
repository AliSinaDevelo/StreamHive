package main

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenLifecycleRuntimeRefusesMissingOrCorruptFileStoreBlob(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, storeDir string, store *storage.FileStore, key []byte)
		wantError error
	}{
		{
			name: "missing raw blob",
			mutate: func(t *testing.T, _ string, store *storage.FileStore, key []byte) {
				require.NoError(t, store.Delete(context.Background(), key))
			},
			wantError: lifecycle.ErrLifecycleBlobMissing,
		},
		{
			name: "corrupt raw blob",
			mutate: func(t *testing.T, storeDir string, _ *storage.FileStore, key []byte) {
				require.NoError(t, os.WriteFile(filepath.Join(storeDir, hex.EncodeToString(key)), []byte("tampered"), 0o600))
			},
			wantError: storage.ErrSHA256Mismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			storeDir := t.TempDir()
			lifecycleDir := t.TempDir()
			store, err := storage.NewFileStore(storeDir)
			require.NoError(t, err)
			config := testLifecycleCLIConfig(lifecycleDir)

			runtime, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
			require.NoError(t, err)
			data := []byte("durable lifecycle value")
			record, _, err := runtime.put(ctx, "demo", "item", data, nil)
			require.NoError(t, err)
			require.NoError(t, runtime.Close())

			tt.mutate(t, storeDir, store, record.BlobKey)
			reopened, err := storage.NewFileStore(storeDir)
			require.NoError(t, err)

			_, err = openLifecycleRuntime(ctx, config, reopened, "token", "node-a")
			require.Error(t, err)
			assert.True(t, errors.Is(err, tt.wantError), "error = %v", err)
		})
	}
}
