package p2p

import (
	"errors"
	"log/slog"
	"net"
	"sync"
)

// ErrAddrRequired is returned when ListenAddress is empty.
var ErrAddrRequired = errors.New("p2p: listen address is required")

// ErrAlreadyListening is returned when ListenAndAccept is called more than once.
var ErrAlreadyListening = errors.New("p2p: already listening")

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
	ListenAddress string
	Listener      net.Listener
	OnPeer        func(Peer)
	Logger        *slog.Logger

	mu    sync.RWMutex
	peers map[string]Peer
}

// NewTCPTransport constructs a transport; ListenAddress must be non-empty before ListenAndAccept.
func NewTCPTransport(listenAddr string) *TCPTransport {
	return &TCPTransport{
		ListenAddress: listenAddr,
		peers:         make(map[string]Peer),
	}
}

func (t *TCPTransport) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// ListenAndAccept binds TCP and starts accepting connections in the background.
func (t *TCPTransport) ListenAndAccept() error {
	if t.ListenAddress == "" {
		return ErrAddrRequired
	}
	t.mu.Lock()
	if t.Listener != nil {
		t.mu.Unlock()
		return ErrAlreadyListening
	}
	t.mu.Unlock()
	ln, err := net.Listen("tcp", t.ListenAddress)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.Listener = ln
	t.mu.Unlock()
	go t.acceptLoop()
	return nil
}

func (t *TCPTransport) acceptLoop() {
	for {
		t.mu.RLock()
		ln := t.Listener
		t.mu.RUnlock()
		if ln == nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			t.logger().Debug("accept exited", "err", err)
			return
		}
		go t.handlePeer(NewTCPPeer(conn, false))
	}
}

func (t *TCPTransport) handlePeer(p Peer) {
	key := p.RemoteAddr().String()
	t.mu.Lock()
	t.peers[key] = p
	t.mu.Unlock()

	t.logger().Info("peer connected", "remote", key, "outbound", p.IsOutbound())

	if t.OnPeer != nil {
		t.OnPeer(p)
	}
}

// Dial opens an outbound TCP connection and registers the peer.
func (t *TCPTransport) Dial(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
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

// Close shuts down the listener and closes all tracked peers.
func (t *TCPTransport) Close() error {
	t.mu.Lock()
	for _, p := range t.peers {
		_ = p.Close()
	}
	t.peers = make(map[string]Peer)
	ln := t.Listener
	t.Listener = nil
	t.mu.Unlock()

	if ln == nil {
		return nil
	}
	return ln.Close()
}

var _ Transport = (*TCPTransport)(nil)
