package lifecycle

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJournalInstallSnapshotPersistsFloorAndRejectsStaleState(t *testing.T) {
	first := testRecord(1, "a", StateDeleted, nil)
	second := testRecord(2, "b", StateDeleted, nil)
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal, first)
	checkpointPath := filepath.Join(filepath.Dir(path), "installed.checkpoint")
	require.NoError(t, journal.InstallSnapshot(context.Background(), checkpointPath, Checkpoint{
		Watermark: second.Version,
		Records:   []Record{first, second},
	}))
	assert.Equal(t, second.Version, journal.Floor())
	assert.Equal(t, second.Version, journal.LastVersion())
	assert.Empty(t, mustJournalRecords(t, journal))

	loaded, err := LoadCheckpoint(context.Background(), checkpointPath, Limits{})
	require.NoError(t, err)
	assert.Equal(t, Checkpoint{Watermark: second.Version, Records: []Record{first, second}}, loaded)
	assert.ErrorIs(t, journal.InstallSnapshot(context.Background(), checkpointPath, Checkpoint{
		Watermark: first.Version,
		Records:   []Record{first},
	}), ErrSnapshotStale)
	require.NoError(t, journal.Close())

	reopened, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	assert.Equal(t, second.Version, recovery.Floor)
	assert.Equal(t, second.Version, recovery.LastVersion)
	assert.Equal(t, 0, recovery.Entries)
	require.NoError(t, reopened.Append(context.Background(), testRecord(3, "c", StateDeleted, nil)))
}

func TestApplierSnapshotChecksBlobsBeforePublishing(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	state := NewStore(Limits{})
	blobs := storage.NewMemoryStore()
	applier, err := NewApplier(blobs, state, journal, Limits{})
	require.NoError(t, err)
	record := testRecord(1, "present", StatePresent, []byte("snapshot-value"))
	checkpointPath := filepath.Join(t.TempDir(), "snapshot.checkpoint")

	err = applier.ApplySnapshot(context.Background(), []string{LifecycleCapabilityV1}, Checkpoint{
		Watermark: record.Version,
		Records:   []Record{record},
	}, checkpointPath)
	assert.ErrorIs(t, err, ErrLifecycleBlobMissing)
	assert.Equal(t, Version{}, journal.Floor())
	assert.Equal(t, 0, journal.Len())
	_, ok, err := state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, blobs.Put(context.Background(), record.BlobKey, []byte("snapshot-value")))
	require.NoError(t, applier.ApplySnapshot(context.Background(), []string{LifecycleCapabilityV1}, Checkpoint{
		Watermark: record.Version,
		Records:   []Record{record},
	}, checkpointPath))
	assert.Equal(t, record.Version, journal.Floor())
	got, ok, err := state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record, got)
}
