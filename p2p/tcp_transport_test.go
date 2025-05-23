package p2p

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTCPTransport(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	require.NotNil(t, tr)
	assert.Equal(t, "127.0.0.1:0", tr.ListenAddress)
	require.NotNil(t, tr.Metrics())
}

func TestListenAndAccept_emptyAddress(t *testing.T) {
	tr := NewTCPTransport("")
	err := tr.ListenAndAccept(context.Background())
	assert.ErrorIs(t, err, ErrAddrRequired)
}

func TestListenAndAccept_invalidAddress(t *testing.T) {
	tr := NewTCPTransport("not-a-host:999999")
	err := tr.ListenAndAccept(context.Background())
	require.Error(t, err)
}

func TestListenAndAccept_contextCancelled(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tr.ListenAndAccept(ctx)
	require.Error(t, err)
}

func TestListenAndAccept_concurrent(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- tr.ListenAndAccept(context.Background())
		}()
	}
	wg.Wait()
	defer func() { _ = tr.Close() }()
	var ok, dup int
	for range n {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyListening):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, n-1, dup)
}

func TestListenAndAccept_twice(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept(context.Background()))
	defer func() { _ = tr.Close() }()
	assert.ErrorIs(t, tr.ListenAndAccept(context.Background()), ErrAlreadyListening)
}

func TestListenAndAccept_setsAddr(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept(context.Background()))
	defer func() { _ = tr.Close() }()

	addr := tr.Addr()
	require.NotNil(t, addr)
	tcpAddr, ok := addr.(*net.TCPAddr)
	require.True(t, ok)
	assert.Greater(t, tcpAddr.Port, 0)
	assert.True(t, tr.Ready())
}

func TestDial_registersOutboundPeer(t *testing.T) {
	ctx := context.Background()
	var serverSeen atomic.Int32
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.OnPeer = func(p Peer) {
		assert.False(t, p.IsOutbound())
		serverSeen.Add(1)
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	var clientSeen atomic.Int32
	client := NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client.OnPeer = func(p Peer) {
		assert.True(t, p.IsOutbound())
		clientSeen.Add(1)
	}
	require.NoError(t, client.ListenAndAccept(ctx))
	defer func() { _ = client.Close() }()

	require.NoError(t, client.Dial(ctx, server.Addr().String()))

	waitFor(t, func() bool {
		return serverSeen.Load() == 1 && clientSeen.Load() == 1
	})
}

func TestDial_contextCancelled(t *testing.T) {
	ctx := context.Background()
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	client := NewTCPTransport("127.0.0.1:0")
	dctx, cancel := context.WithCancel(ctx)
	cancel()
	err := client.Dial(dctx, server.Addr().String())
	require.Error(t, err)
}

func TestDial_afterClose(t *testing.T) {
	ctx := context.Background()
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept(ctx))
	require.NoError(t, tr.Close())
	assert.ErrorIs(t, tr.Dial(ctx, "127.0.0.1:1"), ErrTransportClosed)
}

func TestMaxPeers_rejects(t *testing.T) {
	ctx := context.Background()
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.MaxPeers = 1
	var accepted atomic.Int32
	server.OnPeer = func(Peer) { accepted.Add(1) }
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	addr := server.Addr().String()
	c1 := NewTCPTransport("127.0.0.1:0")
	c1.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, c1.ListenAndAccept(ctx))
	defer func() { _ = c1.Close() }()
	require.NoError(t, c1.Dial(ctx, addr))

	c2 := NewTCPTransport("127.0.0.1:0")
	c2.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, c2.ListenAndAccept(ctx))
	defer func() { _ = c2.Close() }()
	require.NoError(t, c2.Dial(ctx, addr))

	waitFor(t, func() bool { return accepted.Load() == 1 })
	waitFor(t, func() bool { return server.Metrics().PeersRejected.Load() >= 1 })
}

func TestClose_stopsAccept(t *testing.T) {
	ctx := context.Background()
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept(ctx))
	require.NoError(t, tr.Close())
	time.Sleep(50 * time.Millisecond)
	assert.Nil(t, tr.Addr())
	assert.False(t, tr.Ready())
}

func TestClose_underConcurrentDial(t *testing.T) {
	ctx := context.Background()
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, server.ListenAndAccept(ctx))
	addr := server.Addr().String()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err == nil {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	require.NoError(t, server.Close())
	wg.Wait()
}

func TestTCPPeer_Close(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	p := NewTCPPeer(b, true)
	require.NoError(t, p.Close())
}

func TestFrameHandler_receivesFrame(t *testing.T) {
	ctx := context.Background()
	srv := NewTCPTransport("127.0.0.1:0")
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var got atomic.Value
	srv.FrameHandler = func(_ context.Context, _ Peer, payload []byte) error {
		got.Store(append([]byte(nil), payload...))
		return nil
	}
	require.NoError(t, srv.ListenAndAccept(ctx))
	defer func() { _ = srv.Close() }()

	addr := srv.Addr().String()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = WriteFrame(c, []byte("ping"), 1024)
		time.Sleep(100 * time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
	waitFor(t, func() bool { return got.Load() != nil })
	assert.Equal(t, []byte("ping"), got.Load().([]byte))
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-tick.C:
			if cond() {
				return
			}
		}
	}
}

func TestTransport_concurrentDial(t *testing.T) {
	ctx := context.Background()
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	var count atomic.Int32
	server.OnPeer = func(Peer) {
		count.Add(1)
		wg.Done()
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	n := 8
	wg.Add(n)
	addr := server.Addr().String()
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			c := NewTCPTransport("127.0.0.1:0")
			c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := c.ListenAndAccept(ctx); err != nil {
				errCh <- err
				return
			}
			defer func() { _ = c.Close() }()
			errCh <- c.Dial(ctx, addr)
		}()
	}
	for i := 0; i < n; i++ {
		require.NoError(t, <-errCh)
	}
	wg.Wait()
	assert.EqualValues(t, n, count.Load())
}

func TestOnPeerDisconnected(t *testing.T) {
	ctx := context.Background()
	srv := NewTCPTransport("127.0.0.1:0")
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var disc atomic.Int32
	srv.OnPeerDisconnected = func(Peer) {
		disc.Add(1)
	}
	require.NoError(t, srv.ListenAndAccept(ctx))
	defer func() { _ = srv.Close() }()

	c, err := net.Dial("tcp", srv.Addr().String())
	require.NoError(t, err)
	require.NoError(t, c.Close())
	waitFor(t, func() bool { return disc.Load() >= 1 })
}
