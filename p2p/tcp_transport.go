package p2p

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ErrAddrRequired is returned when ListenAddress is empty.
var ErrAddrRequired = errors.New("p2p: listen address is required")

// ErrAlreadyListening is returned when ListenAndAccept is called more than once.
var ErrAlreadyListening = errors.New("p2p: already listening")

// ErrTransportClosed is returned when Dial is used after Close.
var ErrTransportClosed = errors.New("p2p: transport closed")

// TCPPeer is a TCP-backed Peer.
type TCPPeer struct {
	conn     net.Conn
	outbound bool
}

// NewTCPPeer wraps a connection as a Peer.
func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{conn: conn, outbound: outbound}
}

// RemoteAddr returns the remote network address.
func (p *TCPPeer) RemoteAddr() net.Addr { return p.conn.RemoteAddr() }

// Close closes the connection.
func (p *TCPPeer) Close() error { return p.conn.Close() }

// IsOutbound reports whether this peer was created from a dial (outbound).
func (p *TCPPeer) IsOutbound() bool { return p.outbound }

// Conn returns the underlying connection for protocol codecs.
func (p *TCPPeer) Conn() net.Conn { return p.conn }

var _ Peer = (*TCPPeer)(nil)

// TCPTransport listens on TCP and tracks connected peers.
type TCPTransport struct {
	ListenAddress      string
	Listener           net.Listener
	OnPeer             func(Peer)
	OnPeerDisconnected func(Peer)
	// FrameHandler, if set, reads length-prefixed frames until error or handler error.
	FrameHandler func(ctx context.Context, peer Peer, payload []byte) error
	Logger       *slog.Logger

	// MaxPeers limits simultaneous peers (0 = unlimited).
	MaxPeers int
	// DialTimeout bounds each Dial when ctx has no earlier deadline (0 = only ctx).
	DialTimeout time.Duration
	// ReadIdleTimeout sets read deadlines on framed or discard read loops (0 = none).
	ReadIdleTimeout time.Duration
	// MaxFrameBytes caps ReadFrame payload size when using FrameHandler (0 = DefaultMaxFrameBytes).
	MaxFrameBytes int

	TLSServerConfig *tls.Config
	TLSClientConfig *tls.Config

	mu      sync.RWMutex
	peers   map[string]Peer
	metrics *TransportMetrics

	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	acceptWg  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

// NewTCPTransport constructs a transport; ListenAddress must be non-empty before ListenAndAccept.
func NewTCPTransport(listenAddr string) *TCPTransport {
	ctx, cancel := context.WithCancel(context.Background())
	return &TCPTransport{
		ListenAddress:  listenAddr,
		peers:          make(map[string]Peer),
		metrics:        NewTransportMetrics(),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
}

// Metrics returns transport counters.
func (t *TCPTransport) Metrics() *TransportMetrics {
	return t.metrics
}

func (t *TCPTransport) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

func (t *TCPTransport) maxFrame() int {
	if t.MaxFrameBytes > 0 {
		return t.MaxFrameBytes
	}
	return DefaultMaxFrameBytes
}

// Ready reports whether the transport has a bound listener.
func (t *TCPTransport) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Listener != nil
}

// ListenAndAccept binds TCP and starts accepting connections in the background.
func (t *TCPTransport) ListenAndAccept(ctx context.Context) error {
	if t.ListenAddress == "" {
		return ErrAddrRequired
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	t.mu.Lock()
	if t.Listener != nil {
		t.mu.Unlock()
		return ErrAlreadyListening
	}
	t.mu.Unlock()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", t.ListenAddress)
	if err != nil {
		return err
	}

	if t.TLSServerConfig != nil {
		ln = tls.NewListener(ln, t.TLSServerConfig)
	}

	select {
	case <-ctx.Done():
		_ = ln.Close()
		return ctx.Err()
	default:
	}

	t.mu.Lock()
	if t.Listener != nil {
		t.mu.Unlock()
		_ = ln.Close()
		return ErrAlreadyListening
	}
	t.Listener = ln
	t.mu.Unlock()

	t.acceptWg.Add(1)
	go t.acceptLoop()
	return nil
}

func (t *TCPTransport) acceptLoop() {
	defer t.acceptWg.Done()

	for {
		t.mu.RLock()
		ln := t.Listener
		t.mu.RUnlock()
		if ln == nil {
			return
		}

		conn, err := ln.Accept()
		if err != nil {
			t.metrics.AcceptErrors.Add(1)
			t.logger().Debug("accept exited", "err", err)
			return
		}

		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
		}

		go t.handlePeer(NewTCPPeer(conn, false))
	}
}

func (t *TCPTransport) handlePeer(tp *TCPPeer) {
	key := tp.RemoteAddr().String()

	t.mu.Lock()
	if _, dup := t.peers[key]; dup {
		t.mu.Unlock()
		_ = tp.Close()
		return
	}
	if t.MaxPeers > 0 && len(t.peers) >= t.MaxPeers {
		t.mu.Unlock()
		t.metrics.PeersRejected.Add(1)
		_ = tp.Close()
		return
	}
	t.peers[key] = tp
	t.metrics.ActivePeers.Add(1)
	t.mu.Unlock()

	if !tp.outbound {
		t.metrics.InboundAccepts.Add(1)
	}

	t.logger().Info("peer connected", "remote", key, "outbound", tp.outbound)

	if t.OnPeer != nil {
		t.OnPeer(tp)
	}

	go t.peerServe(tp)
}

func (t *TCPTransport) unregisterPeer(p Peer) {
	key := p.RemoteAddr().String()
	t.mu.Lock()
	if _, ok := t.peers[key]; ok {
		delete(t.peers, key)
		t.metrics.ActivePeers.Add(-1)
	}
	t.mu.Unlock()

	if t.OnPeerDisconnected != nil {
		t.OnPeerDisconnected(p)
	}
}

func (t *TCPTransport) peerServe(tp *TCPPeer) {
	defer t.unregisterPeer(tp)

	conn := tp.Conn()
	if t.FrameHandler != nil {
		br := bufio.NewReader(conn)
		max := t.maxFrame()
		for {
			select {
			case <-t.shutdownCtx.Done():
				return
			default:
			}
			if t.ReadIdleTimeout > 0 {
				_ = conn.SetReadDeadline(time.Now().Add(t.ReadIdleTimeout))
			}
			payload, err := ReadFrame(br, max)
			if err != nil {
				return
			}
			t.metrics.FramesHandled.Add(1)
			if err := t.FrameHandler(t.shutdownCtx, tp, payload); err != nil {
				t.metrics.FrameHandlerErrs.Add(1)
				return
			}
		}
	}

	if t.ReadIdleTimeout <= 0 {
		_, _ = io.Copy(io.Discard, conn)
		return
	}

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-t.shutdownCtx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(t.ReadIdleTimeout))
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}

// Dial opens an outbound TCP connection and registers the peer.
func (t *TCPTransport) Dial(ctx context.Context, addr string) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}

	t.metrics.DialAttempts.Add(1)

	dialCtx := ctx
	var cancel context.CancelFunc
	if t.DialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, t.DialTimeout)
		defer cancel()
	}

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		t.metrics.DialErrors.Add(1)
		return err
	}

	if t.TLSClientConfig != nil {
		tlsConn := tls.Client(conn, t.TLSClientConfig)
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			_ = tlsConn.Close()
			t.metrics.DialErrors.Add(1)
			return err
		}
		conn = tlsConn
	}

	t.metrics.DialSuccess.Add(1)
	go t.handlePeer(NewTCPPeer(conn, true))
	return nil
}

// Addr returns the bound listen address, or nil if not listening.
func (t *TCPTransport) Addr() net.Addr {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Listener == nil {
		return nil
	}
	return t.Listener.Addr()
}

// Close shuts down the listener, waits for the accept loop, and closes peers.
func (t *TCPTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.shutdownCancel()

		t.mu.Lock()
		peers := make([]Peer, 0, len(t.peers))
		for _, p := range t.peers {
			peers = append(peers, p)
		}
		ln := t.Listener
		t.Listener = nil
		t.mu.Unlock()

		if ln != nil {
			t.closeErr = ln.Close()
		}

		t.acceptWg.Wait()

		for _, p := range peers {
			_ = p.Close()
		}
	})
	return t.closeErr
}

var _ Transport = (*TCPTransport)(nil)
