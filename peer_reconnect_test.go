package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reconnectTestAddr string

func (a reconnectTestAddr) Network() string { return "tcp" }

func (a reconnectTestAddr) String() string { return string(a) }

type reconnectTestPeer struct {
	target string
}

func (p *reconnectTestPeer) RemoteAddr() net.Addr { return reconnectTestAddr("127.0.0.1:4000") }

func (p *reconnectTestPeer) Close() error { return nil }

func (p *reconnectTestPeer) IsOutbound() bool { return true }

func (p *reconnectTestPeer) DialTarget() string { return p.target }

var _ p2p.Peer = (*reconnectTestPeer)(nil)

func TestPeerReconnectorQueuesDisconnectDuringActiveLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := "localhost:7070"
	metrics := &peerReconnectMetrics{}
	reconnector := newPeerReconnector(ctx, nil, []string{target}, time.Millisecond, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics)
	reconnector.dialing[target] = true

	reconnector.OnPeerDisconnected(&reconnectTestPeer{target: target})
	reconnector.OnPeerDisconnected(&reconnectTestPeer{target: target})

	reconnector.mu.Lock()
	pending := reconnector.pending[target]
	active := reconnector.dialing[target]
	reconnector.mu.Unlock()
	assert.True(t, pending)
	assert.True(t, active)
	assert.True(t, reconnector.finishLoop(target))

	reconnector.mu.Lock()
	_, pending = reconnector.pending[target]
	_, active = reconnector.dialing[target]
	reconnector.mu.Unlock()
	assert.False(t, pending)
	assert.False(t, active)
}

func TestPeerReconnectorDoesNotQueueAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	target := "localhost:7070"
	reconnector := newPeerReconnector(ctx, nil, []string{target}, time.Millisecond, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	reconnector.dialing[target] = true
	cancel()

	reconnector.OnPeerDisconnected(&reconnectTestPeer{target: target})

	reconnector.mu.Lock()
	_, pending := reconnector.pending[target]
	reconnector.mu.Unlock()
	require.False(t, pending)
}
