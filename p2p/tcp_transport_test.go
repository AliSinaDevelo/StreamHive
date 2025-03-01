package p2p

import (
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
}

func TestListenAndAccept_emptyAddress(t *testing.T) {
	tr := NewTCPTransport("")
	err := tr.ListenAndAccept()
	assert.ErrorIs(t, err, ErrAddrRequired)
}

func TestListenAndAccept_invalidAddress(t *testing.T) {
	tr := NewTCPTransport("not-a-host:999999")
	err := tr.ListenAndAccept()
	require.Error(t, err)
}

func TestListenAndAccept_twice(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept())
	defer func() { _ = tr.Close() }()
	assert.ErrorIs(t, tr.ListenAndAccept(), ErrAlreadyListening)
}

func TestListenAndAccept_setsAddr(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept())
	defer func() { _ = tr.Close() }()

	addr := tr.Addr()
	require.NotNil(t, addr)
	tcpAddr, ok := addr.(*net.TCPAddr)
	require.True(t, ok)
	assert.Greater(t, tcpAddr.Port, 0)
}

func TestDial_registersOutboundPeer(t *testing.T) {
	var serverSeen atomic.Int32
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.OnPeer = func(p Peer) {
		assert.False(t, p.IsOutbound())
		serverSeen.Add(1)
	}
	require.NoError(t, server.ListenAndAccept())
	defer func() { _ = server.Close() }()

	var clientSeen atomic.Int32
	client := NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client.OnPeer = func(p Peer) {
		assert.True(t, p.IsOutbound())
		clientSeen.Add(1)
	}
	require.NoError(t, client.ListenAndAccept())
	defer func() { _ = client.Close() }()

	require.NoError(t, client.Dial(server.Addr().String()))

	waitFor(t, func() bool {
		return serverSeen.Load() == 1 && clientSeen.Load() == 1
	})
}

func TestClose_stopsAccept(t *testing.T) {
	tr := NewTCPTransport("127.0.0.1:0")
	tr.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, tr.ListenAndAccept())
	require.NoError(t, tr.Close())
	time.Sleep(50 * time.Millisecond)
	assert.Nil(t, tr.Addr())
}

func TestTCPPeer_Close(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	p := NewTCPPeer(b, true)
	require.NoError(t, p.Close())
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
	server := NewTCPTransport("127.0.0.1:0")
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	var count atomic.Int32
	server.OnPeer = func(Peer) {
		count.Add(1)
		wg.Done()
	}
	require.NoError(t, server.ListenAndAccept())
	defer func() { _ = server.Close() }()

	n := 8
	wg.Add(n)
	addr := server.Addr().String()
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			c := NewTCPTransport("127.0.0.1:0")
			c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := c.ListenAndAccept(); err != nil {
				errCh <- err
				return
			}
			defer func() { _ = c.Close() }()
			errCh <- c.Dial(addr)
		}()
	}
	for i := 0; i < n; i++ {
		require.NoError(t, <-errCh)
	}
	wg.Wait()
	assert.EqualValues(t, n, count.Load())
}
