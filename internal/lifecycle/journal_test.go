package lifecycle

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestJournal(t *testing.T, options JournalOptions) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lifecycle.journal")
	journal, _, err := OpenJournal(path, options)
	require.NoError(t, err)
	return journal, path
}

func appendTestRecords(t *testing.T, journal *Journal, records ...Record) {
	t.Helper()
	for _, record := range records {
		require.NoError(t, journal.Append(context.Background(), record))
	}
}

func TestJournalAppendReplayAndRestart(t *testing.T) {
	first := testRecord(1, "a", StatePresent, []byte("one"))
	second := testRecord(2, "a", StateDeleted, nil)
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal, first, second)

	var replayed []Record
	require.NoError(t, journal.Replay(context.Background(), func(record Record) error {
		replayed = append(replayed, record)
		return nil
	}))
	assert.Equal(t, []Record{first, second}, replayed)
	require.NoError(t, journal.Close())

	reopened, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	assert.False(t, recovery.TruncatedTail)
	assert.Equal(t, Version{}, recovery.Floor)
	assert.Equal(t, second.Version, recovery.LastVersion)
	assert.Equal(t, 2, recovery.Entries)
	got, err := reopened.Records(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Record{first, second}, got)
}

func TestJournalRequiresStrictlyIncreasingVersions(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	first := testRecord(2, "a", StateDeleted, nil)
	appendTestRecords(t, journal, first)

	assert.ErrorIs(t, journal.Append(context.Background(), testRecord(1, "b", StateDeleted, nil)), ErrJournalVersionOrder)
	assert.ErrorIs(t, journal.Append(context.Background(), testRecord(2, "b", StateDeleted, nil)), ErrJournalVersionOrder)
}

func TestJournalLimitAndCancellation(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{MaxJournalEntries: 1})
	defer func() { _ = journal.Close() }()
	appendTestRecords(t, journal, testRecord(1, "a", StateDeleted, nil))
	assert.ErrorIs(t, journal.Append(context.Background(), testRecord(2, "b", StateDeleted, nil)), ErrJournalLimit)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, journal.Append(ctx, testRecord(3, "c", StateDeleted, nil)), context.Canceled)
	assert.ErrorIs(t, journal.Replay(ctx, func(Record) error { return nil }), context.Canceled)
}

func TestJournalTruncatedTailRecovery(t *testing.T) {
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal, testRecord(1, "a", StateDeleted, nil))
	require.NoError(t, journal.Close())

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = file.Write([]byte{0, 0, 0, 20, 1, 2})
	require.NoError(t, err)
	require.NoError(t, file.Close())

	recovered, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = recovered.Close() }()
	assert.True(t, recovery.TruncatedTail)
	assert.Equal(t, Version{}, recovery.Floor)
	assert.Equal(t, 1, recovered.Len())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, recovered.Bytes(), int64(len(data)))
}

func TestJournalChecksumFailureFailsClosed(t *testing.T) {
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal,
		testRecord(1, "a", StateDeleted, nil),
		testRecord(2, "b", StateDeleted, nil),
	)
	require.NoError(t, journal.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	firstLength := int(binary.BigEndian.Uint32(data[journalHeaderBytes : journalHeaderBytes+4]))
	checksumOffset := journalHeaderBytes + 4 + firstLength
	data[checksumOffset] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, _, err = OpenJournal(path, JournalOptions{})
	assert.ErrorIs(t, err, ErrJournalChecksum)
}

func TestCheckpointAtomicReplacementAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.checkpoint")
	first := Checkpoint{
		Watermark: Version{Epoch: 1, Sequence: 1},
		Records:   []Record{testRecord(1, "a", StateDeleted, nil)},
	}
	second := Checkpoint{
		Watermark: Version{Epoch: 1, Sequence: 2},
		Records:   []Record{testRecord(2, "a", StateDeleted, nil)},
	}
	require.NoError(t, SaveCheckpoint(context.Background(), path, first, Limits{}))
	require.NoError(t, SaveCheckpoint(context.Background(), path, second, Limits{}))
	loaded, err := LoadCheckpoint(context.Background(), path, Limits{})
	require.NoError(t, err)
	assert.Equal(t, second.Watermark, loaded.Watermark)
	assert.True(t, recordsEqual(second.Records, loaded.Records))

	temporary, err := filepath.Glob(filepath.Join(dir, ".streamhive-lifecycle-checkpoint-*"))
	require.NoError(t, err)
	assert.Empty(t, temporary)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledPath := filepath.Join(dir, "canceled.checkpoint")
	assert.ErrorIs(t, SaveCheckpoint(ctx, canceledPath, first, Limits{}), context.Canceled)
	_, err = os.Stat(canceledPath)
	assert.True(t, os.IsNotExist(err))
}

func TestJournalCompactionHonorsPeerWatermarksAndRestart(t *testing.T) {
	first := testRecord(1, "a", StatePresent, []byte("one"))
	second := testRecord(2, "a", StateDeleted, nil)
	third := testRecord(3, "b", StatePresent, []byte("three"))
	fourth := testRecord(4, "c", StateDeleted, nil)
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal, first, second, third)
	checkpointPath := filepath.Join(filepath.Dir(path), "state.checkpoint")

	behind := CompactionRequest{
		CheckpointPath: checkpointPath,
		Watermark:      second.Version,
		Records:        []Record{second},
		PeerWatermarks: []Version{{Epoch: 1, Sequence: 1}},
	}
	assert.ErrorIs(t, journal.Compact(context.Background(), behind), ErrPeerBehindWatermark)
	assert.Equal(t, Version{}, journal.Floor())
	assert.Equal(t, 3, journal.Len())

	require.NoError(t, journal.Compact(context.Background(), CompactionRequest{
		CheckpointPath: checkpointPath,
		Watermark:      second.Version,
		Records:        []Record{second},
		PeerWatermarks: []Version{{Epoch: 1, Sequence: 2}},
	}))
	checkpoint, err := LoadCheckpoint(context.Background(), checkpointPath, Limits{})
	require.NoError(t, err)
	assert.Equal(t, second.Version, checkpoint.Watermark)
	assert.Equal(t, []Record{second}, checkpoint.Records)
	assert.Equal(t, second.Version, journal.Floor())
	assert.Equal(t, []Record{third}, mustJournalRecords(t, journal))

	require.NoError(t, journal.Append(context.Background(), fourth))
	require.NoError(t, journal.Close())

	reopened, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	assert.Equal(t, second.Version, recovery.Floor)
	assert.Equal(t, fourth.Version, recovery.LastVersion)
	assert.Equal(t, 2, recovery.Entries)

	finalCheckpointPath := filepath.Join(filepath.Dir(path), "final.checkpoint")
	require.NoError(t, reopened.Compact(context.Background(), CompactionRequest{
		CheckpointPath: finalCheckpointPath,
		Watermark:      fourth.Version,
		Records:        []Record{second, third, fourth},
		Base:           &checkpoint,
		PeerWatermarks: []Version{{Epoch: 1, Sequence: 4}},
	}))
	final, err := LoadCheckpoint(context.Background(), finalCheckpointPath, Limits{})
	require.NoError(t, err)
	assert.Equal(t, fourth.Version, final.Watermark)
	assert.Equal(t, []Record{second, third, fourth}, final.Records)
	assert.Equal(t, fourth.Version, reopened.Floor())
	assert.Empty(t, mustJournalRecords(t, reopened))
	require.NoError(t, reopened.Close())

	lastOpen, recovery, err := OpenJournal(path, JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = lastOpen.Close() }()
	assert.Equal(t, fourth.Version, recovery.Floor)
	assert.Equal(t, fourth.Version, recovery.LastVersion)
	assert.Equal(t, 0, recovery.Entries)
	assert.NoError(t, lastOpen.Append(context.Background(), testRecord(5, "d", StateDeleted, nil)))
}

func TestJournalCompactionRejectsUnsafeStateAndBase(t *testing.T) {
	record := testRecord(1, "a", StateDeleted, nil)
	journal, path := openTestJournal(t, JournalOptions{})
	appendTestRecords(t, journal, record)
	checkpointPath := filepath.Join(filepath.Dir(path), "state.checkpoint")

	err := journal.Compact(context.Background(), CompactionRequest{
		CheckpointPath: checkpointPath,
		Watermark:      record.Version,
		Records:        nil,
	})
	assert.ErrorIs(t, err, ErrCompactionUnsafe)
	assert.Equal(t, 1, journal.Len())

	err = journal.Compact(context.Background(), CompactionRequest{
		CheckpointPath: checkpointPath,
		Watermark:      record.Version,
		Records:        []Record{record},
		Base: &Checkpoint{
			Watermark: Version{Epoch: 2, Sequence: 1},
		},
	})
	assert.ErrorIs(t, err, ErrCompactionBaseMismatch)
	require.NoError(t, journal.Close())
}

func TestJournalClosedAndReplayErrors(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	require.NoError(t, journal.Close())
	assert.ErrorIs(t, journal.Append(context.Background(), testRecord(1, "a", StateDeleted, nil)), ErrJournalClosed)
	assert.ErrorIs(t, journal.Replay(context.Background(), func(Record) error { return nil }), ErrJournalClosed)
	assert.ErrorIs(t, journal.Replay(context.Background(), nil), ErrNilApply)
}

func TestJournalRejectsOversizedRecords(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{Limits: Limits{MaxRecordBytes: 32}})
	defer func() { _ = journal.Close() }()
	err := journal.Append(context.Background(), testRecord(1, "a", StateDeleted, nil))
	assert.ErrorIs(t, err, ErrRecordTooLarge)
}

func mustJournalRecords(t *testing.T, journal *Journal) []Record {
	t.Helper()
	records, err := journal.Records(context.Background())
	require.NoError(t, err)
	return records
}

func TestCheckpointChecksumFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.checkpoint")
	require.NoError(t, SaveCheckpoint(context.Background(), path, Checkpoint{
		Watermark: Version{Epoch: 1, Sequence: 1},
		Records:   []Record{testRecord(1, "a", StateDeleted, nil)},
	}, Limits{}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 1
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err = LoadCheckpoint(context.Background(), path, Limits{})
	assert.ErrorIs(t, err, ErrCheckpointChecksum)
}

func TestJournalPropagatesReplayCallbackErrors(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	appendTestRecords(t, journal, testRecord(1, "a", StateDeleted, nil))
	want := errors.New("stop")
	assert.ErrorIs(t, journal.Replay(context.Background(), func(Record) error { return want }), want)
}
