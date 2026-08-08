package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairBatchRoundTripAndDelivery(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	appendTestRecords(t, journal,
		testRecord(1, "a", StateDeleted, nil),
		testRecord(2, "b", StateDeleted, nil),
		testRecord(3, "c", StateDeleted, nil),
	)

	limits := RepairLimits{MaxRecords: 2}
	batch, err := journal.RepairBatch(context.Background(), Version{}, limits)
	require.NoError(t, err)
	assert.Equal(t, RepairBatchMessageType, batch.Type)
	assert.Equal(t, Version{}, batch.From)
	assert.Equal(t, Version{Epoch: 1, Sequence: 2}, batch.To)
	assert.True(t, batch.More)
	assert.Len(t, batch.Records, 2)

	payload, err := EncodeRepairBatch(batch, limits)
	require.NoError(t, err)
	decoded, err := DecodeRepairBatch(payload, limits)
	require.NoError(t, err)
	assert.Equal(t, batch, decoded)

	delivery, err := decoded.Delivery(Version{}, limits)
	require.NoError(t, err)
	assert.Equal(t, RepairDeliveryReady, delivery)
	delivery, err = decoded.Delivery(decoded.To, limits)
	require.NoError(t, err)
	assert.Equal(t, RepairDeliveryDuplicate, delivery)

	gap := decoded
	gap.From = Version{Epoch: 1, Sequence: 1}
	gap.Records = decoded.Records[1:]
	_, err = gap.Delivery(Version{}, limits)
	assert.ErrorIs(t, err, ErrRepairWatermarkMismatch)

	reordered := decoded
	reordered.Records = []Record{decoded.Records[1], decoded.Records[0]}
	_, err = EncodeRepairBatch(reordered, limits)
	assert.ErrorIs(t, err, ErrRepairOrder)
}

func TestRepairBatchAllowsEmptyWatermark(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()

	from := Version{Epoch: 4, Sequence: 9}
	batch, err := journal.RepairBatch(context.Background(), from, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, from, batch.From)
	assert.Equal(t, from, batch.To)
	assert.False(t, batch.More)
	assert.Empty(t, batch.Records)

	payload, err := EncodeRepairBatch(batch, RepairLimits{})
	require.NoError(t, err)
	decoded, err := DecodeRepairBatch(payload, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, batch, decoded)
}

func TestRepairBatchRejectsIndividualAndFrameBounds(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	appendTestRecords(t, journal,
		testRecord(1, "long-key", StateDeleted, nil),
		testRecord(2, "second", StateDeleted, nil),
	)

	_, err := journal.RepairBatch(context.Background(), Version{}, RepairLimits{
		MaxLogicalKeyBytes: len("documents") + len("long-key") - 1,
	})
	assert.ErrorIs(t, err, ErrRepairLimit)

	one, err := journal.RepairBatch(context.Background(), Version{}, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)
	onePayload, err := EncodeRepairBatch(one, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)

	limited, err := journal.RepairBatch(context.Background(), Version{}, RepairLimits{
		MaxFrameBytes: len(onePayload),
	})
	require.NoError(t, err)
	assert.Len(t, limited.Records, 1)
	assert.True(t, limited.More)
}

func TestPlanRepairUsesSnapshotWhenPeerIsBehindFloor(t *testing.T) {
	first := testRecord(1, "a", StatePresent, []byte("one"))
	second := testRecord(2, "a", StateDeleted, nil)
	third := testRecord(3, "b", StateDeleted, nil)
	journal, path := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	appendTestRecords(t, journal, first, second, third)

	checkpointPath := filepath.Join(filepath.Dir(path), "state.checkpoint")
	require.NoError(t, journal.Compact(context.Background(), CompactionRequest{
		CheckpointPath: checkpointPath,
		Watermark:      second.Version,
		Records:        []Record{second},
		PeerWatermarks: []Version{second.Version},
	}))
	checkpoint, err := LoadCheckpoint(context.Background(), checkpointPath, Limits{})
	require.NoError(t, err)

	plan, err := PlanRepair(context.Background(), journal, Version{}, &checkpoint, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, RepairPlanSnapshot, plan.Mode)
	assert.Equal(t, second.Version, plan.To)
	snapshot, err := DecodeRepairSnapshot(plan.Payload, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, second.Version, snapshot.Watermark)
	assert.Equal(t, []Record{second}, snapshot.Records)

	_, err = PlanRepair(context.Background(), journal, Version{}, nil, RepairLimits{})
	assert.ErrorIs(t, err, ErrRepairSnapshotRequired)
	wrong := checkpoint
	wrong.Watermark = Version{Epoch: 1, Sequence: 1}
	_, err = PlanRepair(context.Background(), journal, Version{}, &wrong, RepairLimits{})
	assert.ErrorIs(t, err, ErrRepairSnapshotWatermark)

	tailPlan, err := PlanRepair(context.Background(), journal, second.Version, &checkpoint, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, RepairPlanJournal, tailPlan.Mode)
	assert.Equal(t, third.Version, tailPlan.To)
	assert.False(t, tailPlan.More)
	tail, err := DecodeRepairBatch(tailPlan.Payload, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, []Record{third}, tail.Records)
}

func TestRepairSnapshotNormalizesAndBoundsPayload(t *testing.T) {
	first := testRecord(1, "z", StateDeleted, nil)
	second := testRecord(1, "a", StateDeleted, nil)
	snapshot := RepairSnapshot{
		Watermark: first.Version,
		Records:   []Record{first, second},
	}
	payload, err := EncodeRepairSnapshot(snapshot, RepairLimits{})
	require.NoError(t, err)
	decoded, err := DecodeRepairSnapshot(payload, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, []byte("a"), decoded.Records[0].LogicalKey)
	assert.Equal(t, []byte("z"), decoded.Records[1].LogicalKey)

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &envelope))
	envelope["unexpected"] = json.RawMessage(`true`)
	unknown, err := json.Marshal(envelope)
	require.NoError(t, err)
	assert.ErrorIs(t, mustDecodeSnapshot(unknown), ErrRepairMalformed)

	trailing := append(bytes.Clone(payload), []byte(` {}`)...)
	assert.ErrorIs(t, mustDecodeSnapshot(trailing), ErrRepairMalformed)

	_, err = EncodeRepairSnapshot(snapshot, RepairLimits{MaxRecords: 1})
	assert.ErrorIs(t, err, ErrRepairLimit)
}

func TestRepairValidationHonorsContextAndMessageTypes(t *testing.T) {
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := journal.RepairBatch(canceled, Version{}, RepairLimits{})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = EncodeRepairBatch(RepairBatch{Type: RepairSnapshotMessageType}, RepairLimits{})
	assert.ErrorIs(t, err, ErrRepairMessageType)
	_, err = DecodeRepairSnapshot([]byte(`{"type":"lifecycle.repair.batch"}`), RepairLimits{})
	assert.ErrorIs(t, err, ErrRepairMessageType)
}

func mustDecodeSnapshot(payload []byte) error {
	_, err := DecodeRepairSnapshot(payload, RepairLimits{})
	return err
}

func TestRepairApplyCanReportMissingReferencedBlob(t *testing.T) {
	store := NewStore(Limits{})
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	blobs := storage.NewMemoryStore()
	applier, err := NewApplier(blobs, store, journal, Limits{})
	require.NoError(t, err)
	record := testRecord(1, "missing", StatePresent, []byte("not-present"))

	_, err = applier.Apply(context.Background(), []string{LifecycleCapabilityV1}, record, nil)
	assert.ErrorIs(t, err, ErrLifecycleBlobMissing)
	_, ok, getErr := store.Get(record.Namespace, record.LogicalKey)
	assert.NoError(t, getErr)
	assert.False(t, ok)
}

func TestRepairBatchPayloadIsStableAcrossDecode(t *testing.T) {
	batch := RepairBatch{
		Type: RepairBatchMessageType,
		From: Version{},
		To:   Version{Epoch: 1, Sequence: 1},
		Records: []Record{
			testRecord(1, "key", StateDeleted, nil),
		},
	}
	payload, err := EncodeRepairBatch(batch, RepairLimits{})
	require.NoError(t, err)
	decoded, err := DecodeRepairBatch(payload, RepairLimits{})
	require.NoError(t, err)
	reencoded, err := EncodeRepairBatch(decoded, RepairLimits{})
	require.NoError(t, err)
	assert.Equal(t, payload, reencoded)
}
