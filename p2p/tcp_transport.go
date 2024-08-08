package p2p

import (
	"fmt"
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
	var err error
	t.listener, err := net.Listen("tcp", t.ListenAddress)
	if err != nil {
		panic(err)
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
