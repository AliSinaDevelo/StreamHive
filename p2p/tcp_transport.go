package p2p

import (
	"fmt"
	"net"
	"sync"
)

// represents remote node over tcpo established connection
type TCPPeer struct {
	// conn is the underlying connection to the remote peer
	conn net.Conn

	// if dial a connection and retrieve => outbound = true
	// if accept and retrieve => outbound = false
	outbound bool
}

func NewTCPPeer(conn net.Conn, outbound bool) *TCPPeer {
	return &TCPPeer{
		conn:     conn,
		outbound: outbound,
	}
}

type TCPTransport struct {
	ListenAddress string
	Listener      net.Listener
	peers         map[net.Addr]Peer
	mu            sync.RWMutex
}

func NewTCPTansport(listenAddr string) *TCPTransport {
	return &TCPTransport{
		ListenAddress: listenAddr,
	}
}

func (t *TCPTransport) ListenAndAccept() {
	var err error
	t.listener, err := net.Listen("tcp", t.ListenAddress)
	if err != nil {
		fmt.Println("failed to listen on %s: %v\n", t.ListenAddress, err)
	}
	go t.startAcceptLoop()

	return nil
}

func (t *TCPTranspot) startAcceptLoop() {
	for {
		conn, err := t.Listener.Accept()
		if err != nil {
			panic(err)
		}
		go t.handleConn(conn)
	}
}

func (t *TCPTransport) handleConn(conn net.Conn) {
	fmt.Println("new incoming connection %+v\n", conn)
}
