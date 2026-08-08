package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatermarkBookPersistsMonotonicAcknowledgements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermarks")
	book, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	assert.Equal(t, Version{}, book.Watermark("missing"))

	first := Version{Epoch: 1, Sequence: 4}
	second := Version{Epoch: 1, Sequence: 9}
	require.NoError(t, book.Acknowledge(context.Background(), "peer-a", first))
	require.NoError(t, book.Acknowledge(context.Background(), "peer-a", first))
	assert.Equal(t, first, book.Watermark("peer-a"))
	assert.ErrorIs(t, book.Acknowledge(context.Background(), "peer-a", Version{Epoch: 1, Sequence: 3}), ErrWatermarkRegression)
	require.NoError(t, book.Acknowledge(context.Background(), "peer-a", second))
	require.NoError(t, book.Acknowledge(context.Background(), "peer-b", first))

	snapshot := book.Snapshot()
	assert.Equal(t, map[string]Version{
		"peer-a": second,
		"peer-b": first,
	}, snapshot)
	snapshot["peer-a"] = Version{}
	assert.Equal(t, second, book.Watermark("peer-a"))

	reopened, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	assert.Equal(t, second, reopened.Watermark("peer-a"))
	assert.Equal(t, first, reopened.Watermark("peer-b"))
}

func TestWatermarkBookSupportsForgetAndEmptyAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watermarks")
	book, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	require.NoError(t, book.Acknowledge(context.Background(), "peer-a", Version{}))
	assert.Equal(t, Version{}, book.Watermark("peer-a"))
	require.NoError(t, book.Acknowledge(context.Background(), "peer-a", Version{Epoch: 2, Sequence: 1}))
	require.NoError(t, book.Forget(context.Background(), "peer-a"))
	require.NoError(t, book.Forget(context.Background(), "peer-a"))

	reopened, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	assert.Empty(t, reopened.Snapshot())
}

func TestWatermarkBookBoundsAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded")
	book, err := OpenWatermarkBook(path, WatermarkOptions{
		MaxPeers:       1,
		MaxPeerIDBytes: 3,
	})
	require.NoError(t, err)
	assert.ErrorIs(t, book.Acknowledge(context.Background(), "long", Version{Epoch: 1, Sequence: 1}), ErrWatermarkPeerInvalid)
	require.NoError(t, book.Acknowledge(context.Background(), "one", Version{Epoch: 1, Sequence: 1}))
	assert.ErrorIs(t, book.Acknowledge(context.Background(), "two", Version{Epoch: 1, Sequence: 2}), ErrWatermarkLimit)
	require.NoError(t, book.Acknowledge(context.Background(), "one", Version{Epoch: 1, Sequence: 2}))

	corruptPath := filepath.Join(t.TempDir(), "corrupt")
	require.NoError(t, os.WriteFile(corruptPath, []byte("not-an-envelope"), 0o600))
	_, err = OpenWatermarkBook(corruptPath, WatermarkOptions{})
	assert.ErrorIs(t, err, ErrWatermarkCorrupt)

	payload, err := json.Marshal(watermarkState{Watermarks: map[string]Version{
		"peer-a": {Epoch: 1, Sequence: 1},
	}})
	require.NoError(t, err)
	envelope, err := encodeEnvelope(payload)
	require.NoError(t, err)
	envelope[len(envelope)-1] ^= 0xff
	checksumPath := filepath.Join(t.TempDir(), "checksum")
	require.NoError(t, os.WriteFile(checksumPath, envelope, 0o600))
	_, err = OpenWatermarkBook(checksumPath, WatermarkOptions{})
	assert.ErrorIs(t, err, ErrWatermarkChecksum)

	unknownPayload := []byte(`{"watermarks":{},"future":true}`)
	unknownEnvelope, err := encodeEnvelope(unknownPayload)
	require.NoError(t, err)
	unknownPath := filepath.Join(t.TempDir(), "unknown")
	require.NoError(t, os.WriteFile(unknownPath, unknownEnvelope, 0o600))
	_, err = OpenWatermarkBook(unknownPath, WatermarkOptions{})
	assert.ErrorIs(t, err, ErrWatermarkCorrupt)
}

func TestRepairCoordinatorResumesFromDurableWatermark(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	first := testRecord(1, "a", StateDeleted, nil)
	second := testRecord(2, "b", StateDeleted, nil)
	appendTestRecords(t, journal, first, second)

	path := filepath.Join(t.TempDir(), "watermarks")
	book, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	coordinator, err := NewRepairCoordinator(journal, book, RepairLimits{})
	require.NoError(t, err)

	initial, err := coordinator.Plan(context.Background(), "peer-a", nil)
	require.NoError(t, err)
	assert.Equal(t, Version{}, initial.From)
	assert.Equal(t, second.Version, initial.To)

	require.NoError(t, coordinator.Acknowledge(context.Background(), "peer-a", first.Version))
	resumed, err := coordinator.Plan(context.Background(), "peer-a", nil)
	require.NoError(t, err)
	assert.Equal(t, first.Version, resumed.From)
	assert.Equal(t, []Record{second}, mustDecodeBatch(t, resumed.Payload).Records)

	restartedBook, err := OpenWatermarkBook(path, WatermarkOptions{})
	require.NoError(t, err)
	restarted, err := NewRepairCoordinator(journal, restartedBook, RepairLimits{})
	require.NoError(t, err)
	resumedAfterRestart, err := restarted.Plan(context.Background(), "peer-a", nil)
	require.NoError(t, err)
	assert.Equal(t, resumed.Payload, resumedAfterRestart.Payload)

	require.NoError(t, restarted.Acknowledge(context.Background(), "peer-a", second.Version))
	complete, err := restarted.Plan(context.Background(), "peer-a", nil)
	require.NoError(t, err)
	assert.Equal(t, second.Version, complete.From)
	assert.Equal(t, second.Version, complete.To)
	assert.Empty(t, mustDecodeBatch(t, complete.Payload).Records)

	assert.ErrorIs(t, restarted.Acknowledge(context.Background(), "peer-a", Version{Epoch: 1, Sequence: 3}), ErrRepairAcknowledgement)
}

func TestRepairCoordinatorValidatesPeerAndDependencies(t *testing.T) {
	_, err := NewRepairCoordinator(nil, nil, RepairLimits{})
	assert.ErrorIs(t, err, ErrNilRepairJournal)

	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	book, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	coordinator, err := NewRepairCoordinator(journal, book, RepairLimits{})
	require.NoError(t, err)

	_, err = coordinator.Plan(context.Background(), "", nil)
	assert.ErrorIs(t, err, ErrWatermarkPeerInvalid)
	assert.ErrorIs(t, coordinator.Acknowledge(context.Background(), "peer-a", Version{Epoch: 1, Sequence: 1}), ErrRepairAcknowledgement)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, coordinator.Acknowledge(canceled, "peer-a", Version{}), context.Canceled)
}

func mustDecodeBatch(t *testing.T, payload []byte) RepairBatch {
	t.Helper()
	batch, err := DecodeRepairBatch(payload, RepairLimits{})
	require.NoError(t, err)
	return batch
}
