package p2p

import (
	"net"
	"sync"
)

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
	ln, err := net.Listen("tcp", t.ListenAddress)
	if err != nil {
		panic(err)
	}
	t.Listener = ln
	for {
		conn, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		go t.handleConn(conn)
	}
}
