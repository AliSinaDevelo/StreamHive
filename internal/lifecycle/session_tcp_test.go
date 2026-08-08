package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairSessionConvergesOverAuthenticatedTCP(t *testing.T) {
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
	targetBook, err := OpenWatermarkBook(filepath.Join(t.TempDir(), "target-watermarks"), WatermarkOptions{})
	require.NoError(t, err)
	sourceCoordinator, err := NewRepairCoordinator(sourceJournal, sourceBook, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)
	targetCoordinator, err := NewRepairCoordinator(targetJournal, targetBook, RepairLimits{MaxRecords: 1})
	require.NoError(t, err)
	targetStore := NewStore(Limits{})
	targetApplier, err := NewApplier(nil, targetStore, targetJournal, Limits{})
	require.NoError(t, err)

	var sender atomic.Pointer[RepairSession]
	var receiver atomic.Pointer[RepairSession]
	setupErrors := make(chan error, 2)

	server := p2p.NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.PeerAuthToken = "shared-secret"
	server.PeerAuthCapabilities = []string{p2p.CapabilityLifecycleV1}
	server.OnPeer = func(peer p2p.Peer) {
		tcpPeer := peer.(*p2p.TCPPeer)
		session, err := NewRepairSession(RepairSessionOptions{
			Peer:        tcpPeer,
			Coordinator: targetCoordinator,
			Applier:     targetApplier,
			PeerID:      "source",
		})
		if err != nil {
			setupErrors <- err
			return
		}
		receiver.Store(session)
	}
	server.FrameHandler = func(frameCtx context.Context, _ p2p.Peer, payload []byte) error {
		session := receiver.Load()
		if session == nil {
			return ErrNilRepairSessionApplier
		}
		return session.Handle(frameCtx, payload)
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	client := p2p.NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client.PeerAuthToken = "shared-secret"
	client.PeerAuthCapabilities = []string{p2p.CapabilityLifecycleV1}
	client.OnPeer = func(peer p2p.Peer) {
		tcpPeer := peer.(*p2p.TCPPeer)
		session, err := NewRepairSession(RepairSessionOptions{
			Peer:        tcpPeer,
			Coordinator: sourceCoordinator,
			PeerID:      "target",
		})
		if err != nil {
			setupErrors <- err
			return
		}
		sender.Store(session)
	}
	client.FrameHandler = func(frameCtx context.Context, _ p2p.Peer, payload []byte) error {
		session := sender.Load()
		if session == nil {
			return ErrNilRepairSessionCoordinator
		}
		return session.Handle(frameCtx, payload)
	}
	require.NoError(t, client.ListenAndAccept(ctx))
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Dial(ctx, server.Addr().String()))
	require.Eventually(t, func() bool {
		return sender.Load() != nil && receiver.Load() != nil
	}, 3*time.Second, 10*time.Millisecond)
	select {
	case err := <-setupErrors:
		require.NoError(t, err)
	default:
	}

	session := sender.Load()
	_, err = session.SendNext(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return sourceBook.Watermark("target") == first.Version && targetBook.Watermark("source") == first.Version
	}, 3*time.Second, 10*time.Millisecond)

	_, err = session.SendNext(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return sourceBook.Watermark("target") == second.Version && targetBook.Watermark("source") == second.Version
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, 2, targetJournal.Len())
	for _, record := range []Record{first, second} {
		got, ok, err := targetStore.Get(record.Namespace, record.LogicalKey)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, record, got)
	}
}
