package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/AliSinaDevelo/StreamHive/p2p"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "TCP listen address")
	dial := flag.String("dial", "", "optional peer host:port to dial after listen")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tr := p2p.NewTCPTransport(*listen)
	tr.Logger = log
	tr.OnPeer = func(peer p2p.Peer) {
		log.Info("peer", "remote", peer.RemoteAddr().String(), "outbound", peer.IsOutbound())
	}

	if err := tr.ListenAndAccept(); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}

	addr := tr.Addr()
	if addr == nil {
		log.Error("no listen address")
		os.Exit(1)
	}
	fmt.Printf("listening on %s\n", addr.String())

	if *dial != "" {
		if err := tr.Dial(*dial); err != nil {
			log.Error("dial", "addr", *dial, "err", err)
			os.Exit(1)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = tr.Close()
}
