package p2p

import (
	"context"
	"net"
)

// Peer is a remote endpoint connected over the network.
type Peer interface {
	RemoteAddr() net.Addr
	Close() error
	IsOutbound() bool
}

// Transport listens for inbound peers and can dial outbound connections.
type Transport interface {
	ListenAndAccept(ctx context.Context) error
	Dial(ctx context.Context, address string) error
	Addr() net.Addr
	Close() error
	Ready() bool
	Metrics() *TransportMetrics
}
