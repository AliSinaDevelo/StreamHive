package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairFramesOverTCPRequireNegotiatedCapability(t *testing.T) {
	ctx := context.Background()
	type lifecycleResult struct {
		frame RepairFrame
		err   error
	}
	lifecycleResults := make(chan lifecycleResult, 2)
	rawTypes := make(chan string, 1)

	server := p2p.NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.PeerAuthToken = "shared-secret"
	server.PeerAuthCapabilities = []string{p2p.CapabilityLifecycleV1}
	server.FrameHandler = func(_ context.Context, peer p2p.Peer, payload []byte) error {
		if message, err := replication.Decode(payload, replication.Limits{}); err == nil {
			rawTypes <- message.Type
			return nil
		}
		tcpPeer := peer.(*p2p.TCPPeer)
		frame, err := DecodeRepairFrameForPeer(payload, tcpPeer.AuthCapabilities(), RepairLimits{})
		lifecycleResults <- lifecycleResult{frame: frame, err: err}
		return err
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	batch := RepairBatch{
		Type:    RepairBatchMessageType,
		From:    Version{},
		To:      Version{Epoch: 1, Sequence: 1},
		Records: []Record{testRecord(1, "tcp-key", StateDeleted, nil)},
	}
	payload, err := EncodeRepairFrame(RepairFrame{Batch: &batch}, RepairLimits{})
	require.NoError(t, err)

	capable := p2p.NewTCPTransport("127.0.0.1:0")
	capable.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	capable.PeerAuthToken = "shared-secret"
	capable.PeerAuthCapabilities = []string{p2p.CapabilityLifecycleV1}
	require.NoError(t, capable.ListenAndAccept(ctx))
	defer func() { _ = capable.Close() }()
	require.NoError(t, capable.Dial(ctx, server.Addr().String()))
	require.Eventually(t, func() bool { return len(server.Peers()) == 1 && len(capable.Peers()) == 1 }, 3*time.Second, 10*time.Millisecond)
	capablePeer := capable.Peers()[0].(*p2p.TCPPeer)
	require.NoError(t, capablePeer.WriteFrame(payload, p2p.DefaultMaxFrameBytes))
	select {
	case result := <-lifecycleResults:
		require.NoError(t, result.err)
		require.NotNil(t, result.frame.Batch)
		assert.Equal(t, batch, *result.frame.Batch)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for capability-gated lifecycle frame")
	}

	rawOnly := p2p.NewTCPTransport("127.0.0.1:0")
	rawOnly.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rawOnly.PeerAuthToken = "shared-secret"
	require.NoError(t, rawOnly.ListenAndAccept(ctx))
	defer func() { _ = rawOnly.Close() }()
	require.NoError(t, rawOnly.Dial(ctx, server.Addr().String()))
	require.Eventually(t, func() bool { return len(server.Peers()) == 2 && len(rawOnly.Peers()) == 1 }, 3*time.Second, 10*time.Millisecond)
	rawPeer := rawOnly.Peers()[0].(*p2p.TCPPeer)
	rawPayload, err := replication.EncodeBlobAck([]byte("raw-key"), replication.Limits{})
	require.NoError(t, err)
	require.NoError(t, rawPeer.WriteFrame(rawPayload, p2p.DefaultMaxFrameBytes))
	select {
	case messageType := <-rawTypes:
		assert.Equal(t, replication.MessageTypeBlobAck, messageType)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for raw compatibility frame")
	}

	require.NoError(t, rawPeer.WriteFrame(payload, p2p.DefaultMaxFrameBytes))
	select {
	case result := <-lifecycleResults:
		assert.ErrorIs(t, result.err, ErrLifecycleCapabilityRequired)
		assert.Nil(t, result.frame.Batch)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle refusal")
	}
	require.Eventually(t, func() bool { return len(server.Peers()) == 1 }, 3*time.Second, 10*time.Millisecond)
}
