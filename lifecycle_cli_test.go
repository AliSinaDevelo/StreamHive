package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLifecycleCLIConfig(dir string) lifecycleCLIConfig {
	return lifecycleCLIConfig{
		enabled: true,
		dir:     dir,
		repairLimits: lifecycle.RepairLimits{
			MaxRecords:         2,
			MaxLogicalKeyBytes: 1024,
			MaxMetadataBytes:   4096,
			MaxFrameBytes:      8192,
		},
	}
}

func TestLifecycleCLIConfigRequiresExplicitRuntimeDependencies(t *testing.T) {
	store := storage.NewMemoryStore()

	assert.EqualError(t,
		(lifecycleCLIConfig{dir: t.TempDir()}).validate(nil, "", ""),
		"lifecycle: -lifecycle-dir requires -lifecycle",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(nil, "token", "node-a"),
		"lifecycle: -lifecycle requires -replicate",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(store, "", "node-a"),
		"lifecycle: -lifecycle requires -peer-auth-token",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(store, "token", ""),
		"lifecycle: -lifecycle requires -peer-id",
	)
	assert.NoError(t, testLifecycleCLIConfig(t.TempDir()).validate(store, "token", "node-a"))
}

func TestOpenLifecycleRuntimeRestoresDurableState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := storage.NewMemoryStore()
	config := testLifecycleCLIConfig(dir)
	runtime, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	record := lifecycle.Record{
		Namespace:   []byte("demo"),
		LogicalKey:  []byte("item"),
		State:       lifecycle.StateDeleted,
		Version:     lifecycle.Version{Epoch: 1, Sequence: 1},
		AuthorityID: "node-a",
	}
	_, err = runtime.applier.Apply(ctx, []string{lifecycle.LifecycleCapabilityV1}, record, nil)
	require.NoError(t, err)
	require.NoError(t, runtime.coordinator.Acknowledge(ctx, "peer-b", record.Version))
	require.NoError(t, runtime.Close())

	restarted, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = restarted.Close() }()
	got, ok, err := restarted.state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record, got)
	assert.FileExists(t, filepath.Join(dir, "journal"))
	assert.FileExists(t, filepath.Join(dir, "watermarks"))
}

func TestOpenLifecycleRuntimeRefusesMissingRestoredBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "journal")
	journal, _, err := lifecycle.OpenJournal(journalPath, lifecycle.JournalOptions{})
	require.NoError(t, err)
	record := lifecycle.Record{
		Namespace:   []byte("demo"),
		LogicalKey:  []byte("item"),
		State:       lifecycle.StatePresent,
		BlobKey:     storage.SHA256Key([]byte("missing")),
		Version:     lifecycle.Version{Epoch: 1, Sequence: 1},
		AuthorityID: "node-a",
	}
	require.NoError(t, journal.Append(ctx, record))
	require.NoError(t, journal.Close())

	_, err = openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), storage.NewMemoryStore(), "token", "node-a")
	require.Error(t, err)
	assert.True(t, errors.Is(err, lifecycle.ErrLifecycleBlobMissing))
}
