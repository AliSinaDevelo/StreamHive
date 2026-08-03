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
	conn        net.Conn
	reader      *bufio.Reader
	outbound    bool
	connectedAt time.Time
	authMethod  string
}

// NewTCPPeer wraps a connection as a Peer.
func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		outbound:    outbound,
		connectedAt: time.Now().UTC(),
		authMethod:  PeerAuthMethodNone,
	}
}

// RemoteAddr returns the remote network address.
func (p *TCPPeer) RemoteAddr() net.Addr { return p.conn.RemoteAddr() }

// LocalAddr returns the local network address.
func (p *TCPPeer) LocalAddr() net.Addr { return p.conn.LocalAddr() }

// Close closes the connection.
func (p *TCPPeer) Close() error { return p.conn.Close() }

// IsOutbound reports whether this peer was created from a dial (outbound).
func (p *TCPPeer) IsOutbound() bool { return p.outbound }

// ConnectedAt reports when this peer was registered locally.
func (p *TCPPeer) ConnectedAt() time.Time { return p.connectedAt }

// AuthMethod reports how this peer passed application-level admission.
func (p *TCPPeer) AuthMethod() string { return p.authMethod }

// Conn returns the underlying connection for protocol codecs.
func (p *TCPPeer) Conn() net.Conn { return p.conn }

// WriteFrame writes one StreamHive frame to the peer connection.
func (p *TCPPeer) WriteFrame(payload []byte, maxPayload int) error {
	return WriteFrame(p.conn, payload, maxPayload)
}

var _ Peer = (*TCPPeer)(nil)

const (
	// PeerAuthMethodNone labels peers connected without shared-token admission.
	PeerAuthMethodNone = "none"
	// PeerAuthMethodSharedToken labels peers admitted by the shared-token handshake.
	PeerAuthMethodSharedToken = "shared-token"
)

// PeerSnapshot is a point-in-time description of a connected peer.
type PeerSnapshot struct {
	RemoteAddr  string
	LocalAddr   string
	Outbound    bool
	ConnectedAt time.Time
	AuthMethod  string
}

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
	// PeerAuthToken, when set, requires a shared-token auth handshake before peer registration.
	PeerAuthToken string
	// PeerAuthTimeout bounds the optional auth handshake (0 = DefaultPeerAuthTimeout).
	PeerAuthTimeout time.Duration

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

// Peers returns a snapshot of currently connected peers.
func (t *TCPTransport) Peers() []Peer {
	t.mu.RLock()
	defer t.mu.RUnlock()
	peers := make([]Peer, 0, len(t.peers))
	for _, peer := range t.peers {
		peers = append(peers, peer)
	}
	return peers
}

// PeerSnapshots returns stable metadata for currently connected peers.
func (t *TCPTransport) PeerSnapshots() []PeerSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snapshots := make([]PeerSnapshot, 0, len(t.peers))
	for _, peer := range t.peers {
		snapshot := PeerSnapshot{
			RemoteAddr: peer.RemoteAddr().String(),
			Outbound:   peer.IsOutbound(),
		}
		if tcpPeer, ok := peer.(*TCPPeer); ok {
			snapshot.LocalAddr = tcpPeer.LocalAddr().String()
			snapshot.ConnectedAt = tcpPeer.ConnectedAt()
			snapshot.AuthMethod = tcpPeer.AuthMethod()
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
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

		go t.handleAcceptedConn(conn)
	}
}

func (t *TCPTransport) handleAcceptedConn(conn net.Conn) {
	tp := NewTCPPeer(conn, false)
	if err := t.authenticateInbound(t.shutdownCtx, tp); err != nil {
		t.metrics.PeerAuthFailures.Add(1)
		t.logger().Warn("peer auth rejected", "remote", tp.RemoteAddr().String(), "err", err)
		_ = tp.Close()
		return
	}
	t.handlePeer(tp)
}

func (t *TCPTransport) handlePeer(tp *TCPPeer) {
	key := tp.RemoteAddr().String()
	if t.peerAuthEnabled() {
		tp.authMethod = PeerAuthMethodSharedToken
	}

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

	t.logger().Info("peer connected", "remote", key, "outbound", tp.outbound, "auth_method", tp.authMethod)

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
			payload, err := ReadFrame(tp.reader, max)
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

	tp := NewTCPPeer(conn, true)
	if err := t.authenticateOutbound(dialCtx, tp); err != nil {
		_ = tp.Close()
		t.metrics.PeerAuthFailures.Add(1)
		t.metrics.DialErrors.Add(1)
		return err
	}

	t.metrics.DialSuccess.Add(1)
	go t.handlePeer(tp)
	return nil
}

func (t *TCPTransport) peerAuthEnabled() bool {
	return t.PeerAuthToken != ""
}

func (t *TCPTransport) peerAuthDeadline(ctx context.Context) time.Time {
	timeout := t.PeerAuthTimeout
	if timeout <= 0 {
		timeout = DefaultPeerAuthTimeout
	}
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	return deadline
}

func (t *TCPTransport) withPeerAuthDeadline(ctx context.Context, conn net.Conn) func() {
	if !t.peerAuthEnabled() {
		return func() {}
	}
	_ = conn.SetDeadline(t.peerAuthDeadline(ctx))
	return func() {
		_ = conn.SetDeadline(time.Time{})
	}
}

func (t *TCPTransport) authenticateInbound(ctx context.Context, tp *TCPPeer) error {
	if !t.peerAuthEnabled() {
		return nil
	}
	clearDeadline := t.withPeerAuthDeadline(ctx, tp.Conn())
	defer clearDeadline()

	payload, err := ReadFrame(tp.reader, t.maxFrame())
	if err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	if err := validatePeerAuthPayload(payload, t.PeerAuthToken); err != nil {
		if rejectPayload, encErr := encodePeerAuthReject(); encErr == nil {
			_ = WriteFrame(tp.Conn(), rejectPayload, t.maxFrame())
		}
		return err
	}
	ackPayload, err := encodePeerAuthOK()
	if err != nil {
		return err
	}
	if err := WriteFrame(tp.Conn(), ackPayload, t.maxFrame()); err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	t.metrics.PeerAuthSuccess.Add(1)
	return nil
}

func (t *TCPTransport) authenticateOutbound(ctx context.Context, tp *TCPPeer) error {
	if !t.peerAuthEnabled() {
		return nil
	}
	clearDeadline := t.withPeerAuthDeadline(ctx, tp.Conn())
	defer clearDeadline()

	payload, err := encodePeerAuth(t.PeerAuthToken)
	if err != nil {
		return err
	}
	if err := WriteFrame(tp.Conn(), payload, t.maxFrame()); err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	ackPayload, err := ReadFrame(tp.reader, t.maxFrame())
	if err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	if err := validatePeerAuthAck(ackPayload); err != nil {
		return err
	}
	t.metrics.PeerAuthSuccess.Add(1)
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
