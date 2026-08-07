package lifecycle

import (
	"errors"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRecord(sequence uint64, key string, state LifecycleState, data []byte) Record {
	record := Record{
		Namespace:   []byte("documents"),
		LogicalKey:  []byte(key),
		State:       state,
		Version:     Version{Epoch: 1, Sequence: sequence},
		AuthorityID: "authority-a",
	}
	if state == StatePresent {
		record.BlobKey = storage.SHA256Key(data)
	}
	return record
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		name  string
		left  Version
		right Version
		want  int
	}{
		{name: "epoch wins", left: Version{Epoch: 2, Sequence: 1}, right: Version{Epoch: 1, Sequence: 99}, want: 1},
		{name: "sequence compares in epoch", left: Version{Epoch: 1, Sequence: 1}, right: Version{Epoch: 1, Sequence: 2}, want: -1},
		{name: "equal", left: Version{Epoch: 3, Sequence: 4}, right: Version{Epoch: 3, Sequence: 4}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.left.Compare(tt.right))
		})
	}
}

func TestRecordValidation(t *testing.T) {
	valid := testRecord(1, "key", StatePresent, []byte("value"))
	tests := []struct {
		name   string
		mutate func(*Record)
		limits Limits
		want   error
	}{
		{name: "empty namespace", mutate: func(r *Record) { r.Namespace = nil }, want: ErrNamespaceEmpty},
		{name: "namespace too large", limits: Limits{MaxNamespaceBytes: 2}, want: ErrNamespaceTooLarge},
		{name: "empty logical key", mutate: func(r *Record) { r.LogicalKey = nil }, want: ErrLogicalKeyEmpty},
		{name: "authority control", mutate: func(r *Record) { r.AuthorityID = "node\n" }, want: ErrAuthorityNotPrintable},
		{name: "zero version", mutate: func(r *Record) { r.Version = Version{} }, want: ErrZeroVersion},
		{name: "invalid state", mutate: func(r *Record) { r.State = "unknown" }, want: ErrInvalidState},
		{name: "missing blob", mutate: func(r *Record) { r.BlobKey = nil }, want: ErrInvalidBlobKey},
		{name: "deleted carries blob", mutate: func(r *Record) { r.State = StateDeleted }, want: ErrInvalidBlobKey},
		{name: "record too large", limits: Limits{MaxRecordBytes: 16}, want: ErrRecordTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := valid.clone()
			if tt.mutate != nil {
				tt.mutate(&record)
			}
			err := record.Validate(tt.limits)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	deleted := testRecord(2, "key", StateDeleted, nil)
	assert.NoError(t, deleted.Validate(Limits{}))
	assert.NoError(t, valid.Validate(Limits{}))
}

func TestStoreAppliesOrderingDuplicatesAndConflicts(t *testing.T) {
	store := NewStore(Limits{})
	first := testRecord(1, "key", StatePresent, []byte("one"))
	second := testRecord(2, "key", StateDeleted, nil)
	conflict := testRecord(2, "key", StatePresent, []byte("two"))

	result, err := store.Apply(second)
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, result.Outcome)

	result, err = store.Apply(first)
	require.NoError(t, err)
	assert.Equal(t, OutcomeStale, result.Outcome)
	assert.Equal(t, second.Version, result.Version)

	result, err = store.Apply(second)
	require.NoError(t, err)
	assert.Equal(t, OutcomeDuplicate, result.Outcome)

	_, err = store.Apply(conflict)
	assert.ErrorIs(t, err, ErrConflict)

	got, ok, err := store.Get([]byte("documents"), []byte("key"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, got.equal(second))
}

func TestStoreOwnsRecordBytes(t *testing.T) {
	store := NewStore(Limits{})
	record := testRecord(1, "key", StatePresent, []byte("value"))
	originalNamespace := append([]byte(nil), record.Namespace...)
	originalBlobKey := append([]byte(nil), record.BlobKey...)
	_, err := store.Apply(record)
	require.NoError(t, err)
	record.Namespace[0] = 'x'
	record.BlobKey[0] ^= 0xff

	got, ok, err := store.Get(originalNamespace, []byte("key"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, originalNamespace, got.Namespace)
	assert.Equal(t, originalBlobKey, got.BlobKey)

	got.Namespace[0] = 'y'
	again, ok, err := store.Get(originalNamespace, []byte("key"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, originalNamespace, again.Namespace)
}

func TestStoreSnapshotIsDeterministic(t *testing.T) {
	store := NewStore(Limits{})
	for _, key := range []string{"z", "a", "m"} {
		_, err := store.Apply(testRecord(1, key, StateDeleted, nil))
		require.NoError(t, err)
	}
	snapshot := store.Snapshot()
	require.Len(t, snapshot, 3)
	assert.Equal(t, []byte("a"), snapshot[0].LogicalKey)
	assert.Equal(t, []byte("m"), snapshot[1].LogicalKey)
	assert.Equal(t, []byte("z"), snapshot[2].LogicalKey)
}

func TestCheckpointRecordValidationRejectsDuplicatesAndFutureVersions(t *testing.T) {
	first := testRecord(1, "a", StateDeleted, nil)
	duplicate := first.clone()
	_, _, err := normalizeCheckpoint(Checkpoint{
		Watermark: Version{Epoch: 1, Sequence: 1},
		Records:   []Record{first, duplicate},
	}, Limits{})
	assert.ErrorIs(t, err, ErrDuplicateLogicalKey)

	_, _, err = normalizeCheckpoint(Checkpoint{
		Watermark: Version{Epoch: 1, Sequence: 1},
		Records:   []Record{testRecord(2, "a", StateDeleted, nil)},
	}, Limits{})
	assert.ErrorIs(t, err, ErrConflict)
}

func TestStoreRejectsInvalidLookup(t *testing.T) {
	_, _, err := NewStore(Limits{}).Get(nil, []byte("key"))
	assert.ErrorIs(t, err, ErrNamespaceEmpty)
	assert.True(t, errors.Is(err, ErrNamespaceEmpty))
}
