package lifecycle

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type repairSessionPeer struct {
	mu       sync.Mutex
	caps     []string
	writes   [][]byte
	maxFrame int
}

func (p *repairSessionPeer) AuthCapabilities() []string {
	return append([]string(nil), p.caps...)
}

func (p *repairSessionPeer) WriteFrame(payload []byte, maxFrame int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, append([]byte(nil), payload...))
	p.maxFrame = maxFrame
	return nil
}

func (p *repairSessionPeer) lastWrite(t *testing.T) []byte {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotEmpty(t, p.writes)
	return append([]byte(nil), p.writes[len(p.writes)-1]...)
}

func (p *repairSessionPeer) writeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes)
}

func newRepairSessionPeer() *repairSessionPeer {
	return &repairSessionPeer{caps: []string{LifecycleCapabilityV1}}
}

func TestRepairSessionAppliesBoundedBatchAndPersistsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	first := testRecord(1, "a", StateDeleted, nil)
	second := testRecord(2, "b", StateDeleted, nil)
	sourceJournal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = sourceJournal.Close() }()
	appendTestRecords(t, sourceJournal, first, second)
	targetJournal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = targetJournal.Close() }()

	sourceBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "source-watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	sourceCoordinator, err := NewRepairCoordinator(sourceJournal, sourceBook, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)
	targetBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "target-watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	targetCoordinator, err := NewRepairCoordinator(targetJournal, targetBook, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)

	sourcePeer := newRepairSessionPeer()
	targetPeer := newRepairSessionPeer()
	sender, err := NewRepairSession(RepairSessionOptions{
		Peer:        sourcePeer,
		Coordinator: sourceCoordinator,
		PeerID:      "target",
	})
	require.NoError(t, err)
	targetStore := NewStore(Limits{})
	targetApplier, err := NewApplier(storage.NewMemoryStore(), targetStore, targetJournal, Limits{})
	require.NoError(t, err)
	receiver, err := NewRepairSession(RepairSessionOptions{
		Peer:        targetPeer,
		Coordinator: targetCoordinator,
		Applier:     targetApplier,
		PeerID:      "source",
	})
	require.NoError(t, err)

	plan, err := sender.SendNext(ctx)
	require.NoError(t, err)
	assert.Equal(t, RepairPlanJournal, plan.Mode)
	require.NoError(t, receiver.Handle(ctx, sourcePeer.lastWrite(t)))
	require.NoError(t, sender.Handle(ctx, targetPeer.lastWrite(t)))
	assert.Equal(t, first.Version, sourceBook.Watermark("target"))
	assert.Equal(t, first.Version, targetBook.Watermark("source"))
	assert.Equal(t, 1, targetJournal.Len())

	plan, err = sender.SendNext(ctx)
	require.NoError(t, err)
	assert.Equal(t, []Record{second}, mustDecodeBatch(t, plan.Payload).Records)
	require.NoError(t, receiver.Handle(ctx, sourcePeer.lastWrite(t)))
	require.NoError(t, sender.Handle(ctx, targetPeer.lastWrite(t)))
	assert.Equal(t, second.Version, sourceBook.Watermark("target"))
	assert.Equal(t, second.Version, targetBook.Watermark("source"))
	assert.Equal(t, 2, targetJournal.Len())
	for _, record := range []Record{first, second} {
		got, ok, err := targetStore.Get(record.Namespace, record.LogicalKey)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, record, got)
	}

	restartedBook, err := OpenWatermarkBook(filepath.Join(filepath.Dir(sourceBook.path), "source-watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	restartedCoordinator, err := NewRepairCoordinator(sourceJournal, restartedBook, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)
	restartedSender, err := NewRepairSession(RepairSessionOptions{
		Peer:        sourcePeer,
		Coordinator: restartedCoordinator,
		PeerID:      "target",
	})
	require.NoError(t, err)
	plan, err = restartedSender.SendNext(ctx)
	require.NoError(t, err)
	assert.Equal(t, second.Version, plan.From)
	assert.Empty(t, mustDecodeBatch(t, plan.Payload).Records)
}

func TestRepairSessionDoesNotAcknowledgeMissingBlob(t *testing.T) {
	ctx := context.Background()
	record := testRecord(1, "missing", StatePresent, []byte("payload"))
	sourceJournal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = sourceJournal.Close() }()
	appendTestRecords(t, sourceJournal, record)
	targetJournal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = targetJournal.Close() }()
	sourceBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "source"), WatermarkOptions{})
	require.NoError(t, err)
	targetBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "target"), WatermarkOptions{})
	require.NoError(t, err)
	sourceCoordinator, err := NewRepairCoordinator(sourceJournal, sourceBook, RepairLimits{})
	require.NoError(t, err)
	targetCoordinator, err := NewRepairCoordinator(targetJournal, targetBook, RepairLimits{})
	require.NoError(t, err)
	sourcePeer := newRepairSessionPeer()
	targetPeer := newRepairSessionPeer()
	sender, err := NewRepairSession(RepairSessionOptions{Peer: sourcePeer, Coordinator: sourceCoordinator, PeerID: "target"})
	require.NoError(t, err)
	targetStore := NewStore(Limits{})
	targetApplier, err := NewApplier(storage.NewMemoryStore(), targetStore, targetJournal, Limits{})
	require.NoError(t, err)
	receiver, err := NewRepairSession(RepairSessionOptions{Peer: targetPeer, Coordinator: targetCoordinator, Applier: targetApplier, PeerID: "source"})
	require.NoError(t, err)

	_, err = sender.SendNext(ctx)
	require.NoError(t, err)
	err = receiver.Handle(ctx, sourcePeer.lastWrite(t))
	assert.ErrorIs(t, err, ErrLifecycleBlobMissing)
	assert.Equal(t, Version{}, targetBook.Watermark("source"))
	assert.Equal(t, 0, targetJournal.Len())
	assert.Equal(t, 0, targetPeer.writeCount())
	_, ok, err := targetStore.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestRepairSessionInstallsSnapshotBeforeAcknowledging(t *testing.T) {
	ctx := context.Background()
	first := testRecord(1, "a", StateDeleted, nil)
	second := testRecord(2, "b", StateDeleted, nil)
	third := testRecord(3, "c", StateDeleted, nil)
	sourceJournal, sourcePath := openTestJournal(t, JournalOptions{})
	defer func() { _ = sourceJournal.Close() }()
	appendTestRecords(t, sourceJournal, first, second, third)
	sourceCheckpointPath := filepath.Join(filepath.Dir(sourcePath), "source.checkpoint")
	require.NoError(t, sourceJournal.Compact(ctx, CompactionRequest{
		CheckpointPath: sourceCheckpointPath,
		Watermark:      second.Version,
		Records:        []Record{first, second},
		PeerWatermarks: []Version{second.Version},
	}))
	checkpoint, err := LoadCheckpoint(ctx, sourceCheckpointPath, Limits{})
	require.NoError(t, err)

	targetJournal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = targetJournal.Close() }()
	sourceBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "source"), WatermarkOptions{})
	require.NoError(t, err)
	targetBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "target"), WatermarkOptions{})
	require.NoError(t, err)
	sourceCoordinator, err := NewRepairCoordinator(sourceJournal, sourceBook, RepairLimits{})
	require.NoError(t, err)
	targetCoordinator, err := NewRepairCoordinator(targetJournal, targetBook, RepairLimits{})
	require.NoError(t, err)
	sourcePeer := newRepairSessionPeer()
	targetPeer := newRepairSessionPeer()
	sender, err := NewRepairSession(RepairSessionOptions{
		Peer:        sourcePeer,
		Coordinator: sourceCoordinator,
		PeerID:      "target",
		Snapshot:    &checkpoint,
	})
	require.NoError(t, err)
	targetStore := NewStore(Limits{})
	targetApplier, err := NewApplier(storage.NewMemoryStore(), targetStore, targetJournal, Limits{})
	require.NoError(t, err)
	targetCheckpointPath := filepath.Join(t.TempDir(), "target.checkpoint")
	receiver, err := NewRepairSession(RepairSessionOptions{
		Peer:           targetPeer,
		Coordinator:    targetCoordinator,
		Applier:        targetApplier,
		PeerID:         "source",
		CheckpointPath: targetCheckpointPath,
	})
	require.NoError(t, err)

	plan, err := sender.SendNext(ctx)
	require.NoError(t, err)
	assert.Equal(t, RepairPlanSnapshot, plan.Mode)
	require.NoError(t, receiver.Handle(ctx, sourcePeer.lastWrite(t)))
	assert.Equal(t, second.Version, targetJournal.Floor())
	assert.Equal(t, 0, targetJournal.Len())
	require.NoError(t, sender.Handle(ctx, targetPeer.lastWrite(t)))
	assert.Equal(t, second.Version, sourceBook.Watermark("target"))
	loaded, err := LoadCheckpoint(ctx, targetCheckpointPath, Limits{})
	require.NoError(t, err)
	assert.Equal(t, checkpoint, loaded)

	plan, err = sender.SendNext(ctx)
	require.NoError(t, err)
	assert.Equal(t, RepairPlanJournal, plan.Mode)
	assert.Equal(t, []Record{third}, mustDecodeBatch(t, plan.Payload).Records)
}

func TestRepairSessionRefusesCapabilityBeforeWriting(t *testing.T) {
	peer := &repairSessionPeer{}
	journal, _ := openTestJournal(t, JournalOptions{})
	defer func() { _ = journal.Close() }()
	book, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	coordinator, err := NewRepairCoordinator(journal, book, RepairLimits{})
	require.NoError(t, err)
	session, err := NewRepairSession(RepairSessionOptions{Peer: peer, Coordinator: coordinator, PeerID: "peer"})
	require.NoError(t, err)
	_, err = session.SendNext(context.Background())
	assert.ErrorIs(t, err, ErrLifecycleCapabilityRequired)
	assert.Equal(t, 0, peer.writeCount())
}
