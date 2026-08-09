package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_shutdownGraceRejectsNonPositive(t *testing.T) {
	var out strings.Builder
	err := run(context.Background(), []string{"-shutdown-grace", "0"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-shutdown-grace must be greater than zero")
}

func TestRun_contextCancellationUsesBoundedShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, stderr safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-health", "127.0.0.1:0",
			"-shutdown-grace", "500ms",
		}, &out, &stderr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "listening on") && strings.Contains(stderr.String(), "msg=health")
	}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())

	cancel()
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop after repeated cancellation")
	}

	assert.Contains(t, stderr.String(), `msg="shutdown started"`)
	assert.Contains(t, stderr.String(), `msg="shutdown complete"`)
	assert.Contains(t, stderr.String(), "shutdown_state=2")
	assert.NotContains(t, stderr.String(), "shutdown completed with errors")
}

func TestRun_exitAfterPutCancellationDrainsTransport(t *testing.T) {
	server := p2p.NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, server.ListenAndAccept(context.Background()))
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, stderr safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", server.Addr().String(),
			"-put-key", "pending-ack",
			"-put-data", "value",
			"-exit-after-put",
			"-put-ack-timeout", "10s",
			"-put-retries", "0",
			"-shutdown-grace", "100ms",
		}, &out, &stderr)
	}()

	require.Eventually(t, func() bool {
		return len(server.Peers()) == 1
	}, 3*time.Second, 10*time.Millisecond, "sender did not connect: stdout=%q stderr=%q", out.String(), stderr.String())
	require.Eventually(t, func() bool {
		return strings.Contains(stderr.String(), "replicated blob sent")
	}, 3*time.Second, 10*time.Millisecond, "sender did not write the pending blob: stdout=%q stderr=%q", out.String(), stderr.String())

	started := time.Now()
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "sender stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatal("sender did not stop after pending ACK cancellation")
	}
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Contains(t, stderr.String(), `msg="shutdown started"`)
	assert.Contains(t, stderr.String(), `msg="shutdown complete"`)
}

func TestRun_cancellationStopsDeferredRepairContinuation(t *testing.T) {
	server := p2p.NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	missingPayload, err := replication.EncodeBlobMissing([][]byte{[]byte("a"), []byte("b")}, replication.Limits{})
	require.NoError(t, err)
	firstPut := make(chan struct{})
	writeErr := make(chan error, 1)
	var putCount atomic.Int32
	server.OnPeer = func(peer p2p.Peer) {
		writer, ok := peer.(interface {
			WriteFrame([]byte, int) error
		})
		if !ok {
			writeErr <- errors.New("connected peer does not expose framed writes")
			return
		}
		if err := writer.WriteFrame(missingPayload, p2p.DefaultMaxFrameBytes); err != nil {
			writeErr <- err
		}
	}
	server.FrameHandler = func(_ context.Context, _ p2p.Peer, payload []byte) error {
		msg, err := replication.Decode(payload, replication.Limits{})
		if err != nil {
			return err
		}
		if msg.Type == replication.MessageTypeBlobPut && putCount.Add(1) == 1 {
			close(firstPut)
		}
		return nil
	}
	require.NoError(t, server.ListenAndAccept(context.Background()))
	defer func() { _ = server.Close() }()

	storeDir := t.TempDir()
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	require.NoError(t, store.Put(context.Background(), []byte("a"), []byte("value-a")))
	require.NoError(t, store.Put(context.Background(), []byte("b"), []byte("value-b")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, stderr safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", server.Addr().String(),
			"-replicate",
			"-store-dir", storeDir,
			"-max-repair-bytes", "1",
			"-shutdown-grace", "500ms",
		}, &out, &stderr)
	}()

	select {
	case err := <-writeErr:
		require.NoError(t, err)
	case <-firstPut:
	}
	require.Eventually(t, func() bool {
		return strings.Contains(stderr.String(), "replication repair continuation scheduled")
	}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "stderr=%q", stderr.String())
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop after deferred repair cancellation")
	}

	time.Sleep(2 * repairContinuationDelay)
	assert.Equal(t, int32(1), putCount.Load())
	assert.Contains(t, stderr.String(), `msg="shutdown complete"`)
}

func TestShutdownApplicationDeadlineExpiresAndJoins(t *testing.T) {
	server := p2p.NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var handlerOnce sync.Once
	server.FrameHandler = func(context.Context, p2p.Peer, []byte) error {
		handlerOnce.Do(func() { close(handlerStarted) })
		<-releaseHandler
		return nil
	}
	require.NoError(t, server.ListenAndAccept(context.Background()))
	defer func() { _ = server.Close() }()

	client := p2p.NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, client.ListenAndAccept(context.Background()))
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	require.Eventually(t, func() bool { return len(client.Peers()) == 1 }, time.Second, 5*time.Millisecond)

	writer, ok := client.Peers()[0].(interface {
		WriteFrame([]byte, int) error
	})
	require.True(t, ok, "client peer does not expose framed writes")
	require.NoError(t, writer.WriteFrame([]byte("blocked"), 1024))
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("server handler did not start")
	}

	started := time.Now()
	err := shutdownApplication(50*time.Millisecond, nil, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.GreaterOrEqual(t, server.Metrics().ShutdownDeadlineExpiries.Load(), uint64(1))
	assert.GreaterOrEqual(t, server.Metrics().ShutdownForcedCloses.Load(), uint64(1))

	close(releaseHandler)
	require.Eventually(t, func() bool {
		return server.Metrics().ShutdownTrackedGoroutines.Load() == 0
	}, time.Second, 5*time.Millisecond)
}

func TestPeerReconnector_doesNotScheduleAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconnector := newPeerReconnector(
		ctx,
		p2p.NewTCPTransport("127.0.0.1:0"),
		[]string{"127.0.0.1:1"},
		time.Millisecond,
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconnector.Start()
	reconnector.schedule("127.0.0.1:1", 0)

	reconnector.mu.Lock()
	defer reconnector.mu.Unlock()
	assert.Empty(t, reconnector.dialing)
}
