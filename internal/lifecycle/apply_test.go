package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestApplier(t *testing.T, blobs storage.BlobStore) (*Applier, *Store, *Journal) {
	t.Helper()
	journal, _ := openTestJournal(t, JournalOptions{})
	t.Cleanup(func() { _ = journal.Close() })
	state := NewStore(Limits{})
	applier, err := NewApplier(blobs, state, journal, Limits{})
	require.NoError(t, err)
	return applier, state, journal
}

func TestDecodeRecordForPeerRefusesBeforeMalformedPayloadDecode(t *testing.T) {
	_, err := DecodeRecordForPeer([]byte("not-json"), nil, TransportLimits{})
	assert.ErrorIs(t, err, ErrLifecycleCapabilityRequired)
	assert.NotErrorIs(t, err, ErrLifecycleEnvelopeMalformed)

	record := testTransportRecord()
	payload, err := EncodeRecord(record, TransportLimits{})
	require.NoError(t, err)
	got, err := DecodeRecordForPeer(payload, []string{LifecycleCapabilityV1}, TransportLimits{})
	require.NoError(t, err)
	assert.Equal(t, record, got)
}

func TestApplierRequiresCapabilityWithoutPublishingState(t *testing.T) {
	blobs := storage.NewMemoryStore()
	applier, state, journal := newTestApplier(t, blobs)
	record := testRecord(1, "key", StatePresent, []byte("value"))

	_, err := applier.Apply(context.Background(), nil, record, []byte("value"))
	assert.ErrorIs(t, err, ErrLifecycleCapabilityRequired)
	_, ok, err := state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 0, journal.Len())
	has, err := blobs.Has(context.Background(), record.BlobKey)
	require.NoError(t, err)
	assert.False(t, has)
}

func TestApplierPersistsPresentBlobBeforePublishingState(t *testing.T) {
	blobs := storage.NewMemoryStore()
	applier, state, journal := newTestApplier(t, blobs)
	data := []byte("durable value")
	record := testRecord(1, "key", StatePresent, data)

	result, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, data)
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, result.Outcome)
	assert.Equal(t, 1, journal.Len())

	storedBlob, err := blobs.Get(context.Background(), record.BlobKey)
	require.NoError(t, err)
	assert.Equal(t, data, storedBlob)
	storedRecord, ok, err := state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record, storedRecord)
}

func TestApplierRejectsMissingAndCorruptPresentBlobs(t *testing.T) {
	tests := []struct {
		name        string
		supplied    []byte
		seedCorrupt bool
		want        error
	}{
		{name: "missing", want: ErrLifecycleBlobMissing},
		{name: "supplied corrupt", supplied: []byte("wrong"), want: storage.ErrSHA256Mismatch},
		{name: "stored corrupt", seedCorrupt: true, want: storage.ErrSHA256Mismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blobs := storage.NewMemoryStore()
			applier, state, journal := newTestApplier(t, blobs)
			data := []byte("expected")
			record := testRecord(1, "key", StatePresent, data)
			if tt.seedCorrupt {
				require.NoError(t, blobs.Put(context.Background(), record.BlobKey, []byte("wrong")))
			}

			_, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, tt.supplied)
			assert.ErrorIs(t, err, tt.want)
			_, ok, getErr := state.Get(record.Namespace, record.LogicalKey)
			require.NoError(t, getErr)
			assert.False(t, ok)
			assert.Equal(t, 0, journal.Len())
		})
	}
}

func TestApplierDeletePublishesTombstoneWithoutDeletingRawBlob(t *testing.T) {
	blobs := &countingBlobStore{MemoryStore: storage.NewMemoryStore()}
	applier, state, journal := newTestApplier(t, blobs)
	data := []byte("retained raw data")
	blobKey := storage.SHA256Key(data)
	require.NoError(t, blobs.Put(context.Background(), blobKey, data))
	record := testRecord(1, "key", StateDeleted, nil)

	result, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, nil)
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, result.Outcome)
	assert.Equal(t, int32(0), blobs.deletes.Load())
	assert.Equal(t, 1, journal.Len())
	got, ok, err := state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, StateDeleted, got.State)
	retained, err := blobs.Get(context.Background(), blobKey)
	require.NoError(t, err)
	assert.Equal(t, data, retained)
}

func TestApplierClassifiesDuplicateStaleAndConflictWithoutNewJournalEntries(t *testing.T) {
	applier, state, journal := newTestApplier(t, storage.NewMemoryStore())
	first := testRecord(1, "key", StateDeleted, nil)
	_, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, first, nil)
	require.NoError(t, err)

	result, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, first, nil)
	require.NoError(t, err)
	assert.Equal(t, OutcomeDuplicate, result.Outcome)
	assert.Equal(t, 1, journal.Len())

	stale := testRecord(0, "key", StateDeleted, nil)
	result, err = applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, stale, nil)
	require.NoError(t, err)
	assert.Equal(t, OutcomeStale, result.Outcome)
	assert.Equal(t, 1, journal.Len())

	conflict := testRecord(1, "key", StatePresent, []byte("different"))
	_, err = applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, conflict, []byte("different"))
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, 1, journal.Len())

	newer := testRecord(2, "key", StateDeleted, nil)
	result, err = applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, newer, nil)
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, result.Outcome)
	assert.Equal(t, 2, journal.Len())
	got, ok, err := state.Get(newer.Namespace, newer.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newer.Version, got.Version)
}

func TestApplierReplaysJournalAfterRestart(t *testing.T) {
	blobs := storage.NewMemoryStore()
	journal, path := openTestJournal(t, JournalOptions{})
	state := NewStore(Limits{})
	applier, err := NewApplier(blobs, state, journal, Limits{})
	require.NoError(t, err)
	record := testRecord(1, "key", StateDeleted, nil)
	_, err = applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, nil)
	require.NoError(t, err)
	require.NoError(t, journal.Close())

	reopened, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	assert.Equal(t, 1, recovery.Entries)
	replayed := NewStore(Limits{})
	require.NoError(t, reopened.Replay(context.Background(), func(record Record) error {
		_, err := replayed.Apply(record)
		return err
	}))
	got, ok, err := replayed.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record, got)
}

func TestNewApplierRequiresStateAndJournal(t *testing.T) {
	blobs := storage.NewMemoryStore()
	_, err := NewApplier(blobs, nil, nil, Limits{})
	assert.ErrorIs(t, err, ErrNilApplierState)

	state := NewStore(Limits{})
	_, err = NewApplier(blobs, state, nil, Limits{})
	assert.ErrorIs(t, err, ErrNilApplierJournal)
}

type countingBlobStore struct {
	*storage.MemoryStore
	deletes atomic.Int32
}

func (s *countingBlobStore) Delete(ctx context.Context, key []byte) error {
	s.deletes.Add(1)
	return s.MemoryStore.Delete(ctx, key)
}

var _ storage.BlobStore = (*countingBlobStore)(nil)

func TestApplierDoesNotStoreUnverifiedSuppliedBytes(t *testing.T) {
	blobs := storage.NewMemoryStore()
	applier, _, _ := newTestApplier(t, blobs)
	data := []byte("expected")
	record := testRecord(1, "key", StatePresent, data)

	_, err := applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, bytes.Clone([]byte("tampered")))
	assert.True(t, errors.Is(err, storage.ErrSHA256Mismatch))
	has, err := blobs.Has(context.Background(), record.BlobKey)
	require.NoError(t, err)
	assert.False(t, has)
}
