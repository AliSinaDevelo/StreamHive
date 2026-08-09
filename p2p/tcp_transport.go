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

// ErrDrainDeadlineRequired is returned when Drain is called without a deadline.
var ErrDrainDeadlineRequired = errors.New("p2p: drain deadline is required")

type transportLifecycleState int64

const (
	transportStateOpen transportLifecycleState = iota
	transportStateDraining
	transportStateClosed
	transportDrainJoinGrace = 250 * time.Millisecond
)

// TCPPeer is a TCP-backed Peer.
type TCPPeer struct {
	conn             net.Conn
	reader           *bufio.Reader
	writeMu          sync.Mutex
	outbound         bool
	dialTarget       string
	connectedAt      time.Time
	authMethod       string
	authIdentity     string
	authCapabilities []string
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

// DialTarget returns the configured outbound address for this peer.
// It is empty for inbound peers and peers created outside TCPTransport.Dial.
func (p *TCPPeer) DialTarget() string { return p.dialTarget }

// ConnectedAt reports when this peer was registered locally.
func (p *TCPPeer) ConnectedAt() time.Time { return p.connectedAt }

// AuthMethod reports how this peer passed application-level admission.
func (p *TCPPeer) AuthMethod() string { return p.authMethod }

// AuthIdentity reports the remote application's authenticated identity, when provided.
func (p *TCPPeer) AuthIdentity() string { return p.authIdentity }

// AuthCapabilities reports the capabilities negotiated with the remote application.
func (p *TCPPeer) AuthCapabilities() []string {
	return append([]string(nil), p.authCapabilities...)
}

// HasCapability reports whether the remote application negotiated capability.
func (p *TCPPeer) HasCapability(capability string) bool {
	return HasCapability(p.authCapabilities, capability)
}

// LifecycleCapabilityStatus reports whether lifecycle records may be exchanged with this peer.
func (p *TCPPeer) LifecycleCapabilityStatus(required bool) CapabilityStatus {
	return LifecycleCapabilityStatus(p.authCapabilities, required)
}

// Conn returns the underlying connection for protocol codecs.
func (p *TCPPeer) Conn() net.Conn { return p.conn }

// WriteFrame writes one StreamHive frame to the peer connection.
func (p *TCPPeer) WriteFrame(payload []byte, maxPayload int) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
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
	RemoteAddr   string
	LocalAddr    string
	Outbound     bool
	ConnectedAt  time.Time
	AuthMethod   string
	AuthIdentity string
	Capabilities []string
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
	// PeerAuthIdentity is the optional application identity sent during shared-token auth.
	PeerAuthIdentity string
	// PeerAuthAllowedIdentities restricts inbound shared-token peers to these application identities.
	// An empty list disables identity authorization while retaining token-only compatibility.
	PeerAuthAllowedIdentities []string
	// PeerAuthCapabilities advertises supported capabilities in the shared-token auth envelope.
	// Capabilities are exchanged only when PeerAuthToken is configured.
	PeerAuthCapabilities []string
	// TLSHandshakeTimeout bounds TLS handshakes before peer registration (0 = DefaultTLSHandshakeTimeout).
	TLSHandshakeTimeout time.Duration

	TLSServerConfig *tls.Config
	TLSClientConfig *tls.Config

	mu          sync.RWMutex
	peers       map[string]Peer
	connections map[*TCPPeer]struct{}
	metrics     *TransportMetrics
	workDone    chan struct{}
	workCount   int

	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error
	state     atomic.Int64
}

// NewTCPTransport constructs a transport; ListenAddress must be non-empty before ListenAndAccept.
func NewTCPTransport(listenAddr string) *TCPTransport {
	ctx, cancel := context.WithCancel(context.Background())
	workDone := make(chan struct{})
	close(workDone)
	metrics := NewTransportMetrics()
	metrics.ShutdownState.Store(int64(transportStateOpen))
	return &TCPTransport{
		ListenAddress:  listenAddr,
		peers:          make(map[string]Peer),
		connections:    make(map[*TCPPeer]struct{}),
		metrics:        metrics,
		workDone:       workDone,
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
			snapshot.AuthIdentity = tcpPeer.AuthIdentity()
			snapshot.Capabilities = tcpPeer.AuthCapabilities()
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

func (t *TCPTransport) lifecycleState() transportLifecycleState {
	return transportLifecycleState(t.state.Load())
}

func (t *TCPTransport) startWorkLocked() bool {
	if t.lifecycleState() != transportStateOpen {
		return false
	}
	if t.workCount == 0 {
		t.workDone = make(chan struct{})
	}
	t.workCount++
	t.metrics.ShutdownTrackedGoroutines.Add(1)
	return true
}

func (t *TCPTransport) beginWork() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.startWorkLocked()
}

func (t *TCPTransport) finishWork() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.workCount == 0 {
		return
	}
	t.workCount--
	t.metrics.ShutdownTrackedGoroutines.Add(-1)
	if t.workCount == 0 {
		close(t.workDone)
	}
}

func (t *TCPTransport) trackConnection(tp *TCPPeer) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lifecycleState() != transportStateOpen {
		return false
	}
	t.connections[tp] = struct{}{}
	return true
}

func (t *TCPTransport) removeConnection(tp *TCPPeer) {
	t.mu.Lock()
	delete(t.connections, tp)
	t.mu.Unlock()
}

func (t *TCPTransport) registeredConnectionLocked(tp *TCPPeer) bool {
	peer, ok := t.peers[tp.RemoteAddr().String()]
	return ok && peer == tp
}

func (t *TCPTransport) snapshotConnections() (pending, active []*TCPPeer) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for tp := range t.connections {
		if t.registeredConnectionLocked(tp) {
			active = append(active, tp)
		} else {
			pending = append(pending, tp)
		}
	}
	return pending, active
}

func (t *TCPTransport) closeConnections(peers []*TCPPeer) int {
	closed := 0
	for _, peer := range peers {
		if peer == nil {
			continue
		}
		_ = peer.Close()
		closed++
	}
	return closed
}

func (t *TCPTransport) waitForWork(ctx context.Context) error {
	t.mu.RLock()
	done := t.workDone
	t.mu.RUnlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *TCPTransport) markClosed() {
	t.state.Store(int64(transportStateClosed))
	t.metrics.ShutdownState.Store(int64(transportStateClosed))
}

// Ready reports whether the transport has a bound listener.
func (t *TCPTransport) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Listener != nil
}

// ListenAndAccept binds TCP and starts accepting connections in the background.
func (t *TCPTransport) ListenAndAccept(ctx context.Context) error {
	if err := t.validatePeerAuthConfig(); err != nil {
		return err
	}
	if t.ListenAddress == "" {
		return ErrAddrRequired
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	t.mu.Lock()
	if t.lifecycleState() != transportStateOpen {
		t.mu.Unlock()
		return ErrTransportClosed
	}
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
	if t.lifecycleState() != transportStateOpen {
		t.mu.Unlock()
		_ = ln.Close()
		return ErrTransportClosed
	}
	if t.Listener != nil {
		t.mu.Unlock()
		_ = ln.Close()
		return ErrAlreadyListening
	}
	t.Listener = ln
	if !t.startWorkLocked() {
		t.Listener = nil
		t.mu.Unlock()
		_ = ln.Close()
		return ErrTransportClosed
	}
	t.mu.Unlock()

	go t.acceptLoop()
	return nil
}

func (t *TCPTransport) acceptLoop() {
	defer t.finishWork()

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

		tp := NewTCPPeer(conn, false)
		t.mu.Lock()
		admitted := t.lifecycleState() == transportStateOpen && t.startWorkLocked()
		if admitted {
			t.connections[tp] = struct{}{}
		}
		t.mu.Unlock()
		if !admitted {
			_ = tp.Close()
			continue
		}
		go t.handleAcceptedConn(tp)
	}
}

func (t *TCPTransport) handleAcceptedConn(tp *TCPPeer) {
	registered := false
	defer func() {
		if !registered {
			t.removeConnection(tp)
		}
		t.finishWork()
	}()

	if tlsConn, ok := tp.Conn().(*tls.Conn); ok {
		handshakeCtx, cancel := t.tlsHandshakeContext(t.shutdownCtx)
		err := tlsConn.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			t.metrics.TLSHandshakeFailures.Add(1)
			t.logger().Warn("tls handshake rejected", "remote", tp.RemoteAddr().String(), "err", err)
			_ = tp.Close()
			return
		}
		t.metrics.TLSHandshakeSuccess.Add(1)
	}
	if err := t.authenticateInbound(t.shutdownCtx, tp); err != nil {
		t.metrics.PeerAuthFailures.Add(1)
		t.logger().Warn("peer auth rejected", "remote", tp.RemoteAddr().String(), "err", err)
		_ = tp.Close()
		return
	}
	registered, _ = t.handlePeer(tp)
	if !registered {
		_ = tp.Close()
	}
}

func (t *TCPTransport) handlePeer(tp *TCPPeer) (bool, error) {
	key := tp.RemoteAddr().String()
	if t.peerAuthEnabled() {
		tp.authMethod = PeerAuthMethodSharedToken
	}

	t.mu.Lock()
	if t.lifecycleState() != transportStateOpen {
		t.mu.Unlock()
		return false, ErrTransportClosed
	}
	if _, dup := t.peers[key]; dup {
		t.mu.Unlock()
		_ = tp.Close()
		return false, nil
	}
	if t.MaxPeers > 0 && len(t.peers) >= t.MaxPeers {
		t.mu.Unlock()
		t.metrics.PeersRejected.Add(1)
		_ = tp.Close()
		return false, nil
	}
	if !t.startWorkLocked() {
		t.mu.Unlock()
		return false, ErrTransportClosed
	}
	t.peers[key] = tp
	t.metrics.ActivePeers.Add(1)
	t.metrics.ShutdownTrackedPeers.Add(1)
	t.mu.Unlock()

	if !tp.outbound {
		t.metrics.InboundAccepts.Add(1)
	}

	t.logger().Info("peer connected", "remote", key, "outbound", tp.outbound, "auth_method", tp.authMethod, "auth_identity", tp.authIdentity)

	if t.OnPeer != nil {
		t.OnPeer(tp)
	}

	go t.peerServe(tp)
	return true, nil
}

func (t *TCPTransport) unregisterPeer(p Peer) {
	key := p.RemoteAddr().String()
	t.mu.Lock()
	if _, ok := t.peers[key]; ok {
		delete(t.peers, key)
		t.metrics.ActivePeers.Add(-1)
		t.metrics.ShutdownTrackedPeers.Add(-1)
	}
	t.mu.Unlock()
	if tp, ok := p.(*TCPPeer); ok {
		t.removeConnection(tp)
	}

	if t.OnPeerDisconnected != nil {
		t.OnPeerDisconnected(p)
	}
}

func (t *TCPTransport) peerServe(tp *TCPPeer) {
	defer t.finishWork()
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
	if err := t.validatePeerAuthConfig(); err != nil {
		return err
	}
	if !t.beginWork() {
		return ErrTransportClosed
	}
	defer t.finishWork()

	t.metrics.DialAttempts.Add(1)

	dialCtx, cancel := context.WithCancel(ctx)
	stopShutdown := context.AfterFunc(t.shutdownCtx, cancel)
	defer stopShutdown()
	defer cancel()
	if t.DialTimeout > 0 {
		var timeoutCancel context.CancelFunc
		dialCtx, timeoutCancel = context.WithTimeout(dialCtx, t.DialTimeout)
		defer timeoutCancel()
	}

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		t.metrics.DialErrors.Add(1)
		if t.lifecycleState() != transportStateOpen {
			return ErrTransportClosed
		}
		return err
	}
	tp := NewTCPPeer(conn, true)
	tp.dialTarget = addr
	if !t.trackConnection(tp) {
		_ = tp.Close()
		t.metrics.DialErrors.Add(1)
		return ErrTransportClosed
	}
	transferred := false
	defer func() {
		if !transferred {
			t.removeConnection(tp)
		}
	}()

	if t.TLSClientConfig != nil {
		tlsConn := tls.Client(conn, t.TLSClientConfig)
		handshakeCtx, cancelHandshake := t.tlsHandshakeContext(dialCtx)
		err := tlsConn.HandshakeContext(handshakeCtx)
		cancelHandshake()
		if err != nil {
			_ = tlsConn.Close()
			t.metrics.TLSHandshakeFailures.Add(1)
			t.metrics.DialErrors.Add(1)
			return err
		}
		t.metrics.TLSHandshakeSuccess.Add(1)
		tp.conn = tlsConn
		tp.reader = bufio.NewReader(tlsConn)
	}

	if err := t.authenticateOutbound(dialCtx, tp); err != nil {
		_ = tp.Close()
		t.metrics.PeerAuthFailures.Add(1)
		t.metrics.DialErrors.Add(1)
		return err
	}

	t.metrics.DialSuccess.Add(1)
	registered, err := t.handlePeer(tp)
	if err != nil {
		_ = tp.Close()
		t.metrics.DialErrors.Add(1)
		return err
	}
	if !registered {
		return nil
	}
	transferred = true
	return nil
}

func (t *TCPTransport) peerAuthEnabled() bool {
	return t.PeerAuthToken != ""
}

func (t *TCPTransport) validatePeerAuthConfig() error {
	if (t.PeerAuthIdentity != "" || len(t.PeerAuthAllowedIdentities) > 0) && !t.peerAuthEnabled() {
		return ErrPeerAuthIdentityRequiresToken
	}
	if len(t.PeerAuthCapabilities) > 0 && !t.peerAuthEnabled() {
		return ErrPeerAuthCapabilitiesRequiresToken
	}
	if err := validatePeerIdentity(t.PeerAuthIdentity); err != nil {
		return err
	}
	if err := validatePeerAuthAllowlist(t.PeerAuthAllowedIdentities); err != nil {
		return err
	}
	_, err := normalizePeerAuthCapabilities(t.PeerAuthCapabilities, true)
	return err
}

func (t *TCPTransport) maxPeerAuthFrame() int {
	max := t.maxFrame()
	if max > MaxPeerAuthPayloadBytes {
		return MaxPeerAuthPayloadBytes
	}
	return max
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

func (t *TCPTransport) tlsHandshakeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := t.TLSHandshakeTimeout
	if timeout <= 0 {
		timeout = DefaultTLSHandshakeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (t *TCPTransport) authenticateInbound(ctx context.Context, tp *TCPPeer) error {
	if !t.peerAuthEnabled() {
		return nil
	}
	clearDeadline := t.withPeerAuthDeadline(ctx, tp.Conn())
	defer clearDeadline()

	payload, err := ReadFrame(tp.reader, t.maxPeerAuthFrame())
	if err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	identity, capabilities, err := validatePeerAuthPayload(payload, t.PeerAuthToken, t.PeerAuthAllowedIdentities)
	if err != nil {
		if errors.Is(err, ErrPeerAuthIdentityInvalid) || errors.Is(err, ErrPeerAuthIdentityNotAllowed) {
			t.metrics.PeerAuthIdentityRejections.Add(1)
		}
		if rejectPayload, encErr := encodePeerAuthReject(); encErr == nil {
			_ = WriteFrame(tp.Conn(), rejectPayload, t.maxPeerAuthFrame())
		}
		return err
	}
	tp.authIdentity = identity
	tp.authCapabilities = capabilities
	ackPayload, err := encodePeerAuthOK(t.PeerAuthIdentity, t.PeerAuthCapabilities)
	if err != nil {
		return err
	}
	if err := WriteFrame(tp.Conn(), ackPayload, t.maxPeerAuthFrame()); err != nil {
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

	payload, err := encodePeerAuth(t.PeerAuthToken, t.PeerAuthIdentity, t.PeerAuthCapabilities)
	if err != nil {
		return err
	}
	if err := WriteFrame(tp.Conn(), payload, t.maxPeerAuthFrame()); err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	ackPayload, err := ReadFrame(tp.reader, t.maxPeerAuthFrame())
	if err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	identity, capabilities, err := validatePeerAuthAck(ackPayload)
	if err != nil {
		return err
	}
	tp.authIdentity = identity
	tp.authCapabilities = capabilities
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

func (t *TCPTransport) beginShutdown() (net.Listener, int, []*TCPPeer, []*TCPPeer) {
	t.mu.Lock()
	t.state.Store(int64(transportStateDraining))
	t.metrics.ShutdownState.Store(int64(transportStateDraining))
	t.metrics.ShutdownsStarted.Add(1)
	t.shutdownCancel()

	listener := t.Listener
	t.Listener = nil
	peerCount := len(t.peers)
	var pending, active []*TCPPeer
	for tp := range t.connections {
		if t.registeredConnectionLocked(tp) {
			active = append(active, tp)
		} else {
			pending = append(pending, tp)
		}
	}
	t.mu.Unlock()
	return listener, peerCount, pending, active
}

func (t *TCPTransport) completeShutdown(peerCount int) {
	if peerCount > 0 {
		t.metrics.ShutdownPeersDrained.Add(uint64(peerCount))
	}
	t.markClosed()
}

func (t *TCPTransport) drain(ctx context.Context) error {
	listener, peerCount, pending, active := t.beginShutdown()
	var listenerErr error
	if listener != nil {
		listenerErr = listener.Close()
	}

	if forced := t.closeConnections(pending); forced > 0 {
		t.metrics.ShutdownForcedCloses.Add(uint64(forced))
	}
	deadline, _ := ctx.Deadline()
	for _, peer := range active {
		_ = peer.Conn().SetDeadline(deadline)
	}

	if err := t.waitForWork(ctx); err == nil {
		t.completeShutdown(peerCount)
		return listenerErr
	} else {
		t.metrics.ShutdownDeadlineExpiries.Add(1)
		pending, active = t.snapshotConnections()
		if forced := t.closeConnections(append(pending, active...)); forced > 0 {
			t.metrics.ShutdownForcedCloses.Add(uint64(forced))
		}
		joinCtx, cancel := context.WithTimeout(context.Background(), transportDrainJoinGrace)
		joinErr := t.waitForWork(joinCtx)
		cancel()
		if joinErr != nil {
			t.logger().Warn("transport drain deadline expired with tracked work", "err", joinErr, "tracked_goroutines", t.metrics.ShutdownTrackedGoroutines.Load())
		}
		t.completeShutdown(peerCount)
		return errors.Join(listenerErr, err)
	}
}

// Drain stops new admissions, gives cooperative peer work until ctx's deadline,
// and force-closes remaining sockets when the deadline expires.
func (t *TCPTransport) Drain(ctx context.Context) error {
	if ctx == nil {
		return ErrDrainDeadlineRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrDrainDeadlineRequired
	}
	t.closeOnce.Do(func() {
		t.closeErr = t.drain(ctx)
	})
	return t.closeErr
}

// Close is the immediate, idempotent hard-stop path. Use Drain for a bounded
// cooperative shutdown.
func (t *TCPTransport) Close() error {
	t.closeOnce.Do(func() {
		listener, peerCount, pending, active := t.beginShutdown()
		if listener != nil {
			t.closeErr = listener.Close()
		}
		if forced := t.closeConnections(append(pending, active...)); forced > 0 {
			t.metrics.ShutdownForcedCloses.Add(uint64(forced))
		}
		t.completeShutdown(peerCount)
	})
	return t.closeErr
}

var _ Transport = (*TCPTransport)(nil)
