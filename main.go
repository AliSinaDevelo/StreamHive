package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AliSinaDevelo/StreamHive/internal/version"
	"github.com/AliSinaDevelo/StreamHive/p2p"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("streamhive", flag.ContinueOnError)
	fs.SetOutput(stderr)

	listen := fs.String("listen", "127.0.0.1:0", "TCP listen address")
	dial := fs.String("dial", "", "optional peer host:port to dial after listen")
	health := fs.String("health", "", "optional HTTP listen addr for /livez /readyz /metrics (e.g. :8080)")
	maxPeers := fs.Int("max-peers", 0, "max simultaneous peers (0 = unlimited)")
	dialTimeout := fs.Duration("dial-timeout", 0, "default dial timeout (0 = use context only)")
	readIdle := fs.Duration("read-idle-timeout", 0, "TCP read deadline refresh for peer loops (0 = none for discard mode)")
	showVer := fs.Bool("version", false, "print version and exit")

	tlsCert := fs.String("tls-cert", "", "path to PEM certificate (enables TLS on listener)")
	tlsKey := fs.String("tls-key", "", "path to PEM private key for -tls-cert")
	tlsCA := fs.String("tls-ca", "", "optional path to PEM CA bundle for outbound TLS")
	tlsServerName := fs.String("tls-server-name", "", "SNI / cert verification name for outbound TLS")
	insecureSkip := fs.Bool("tls-insecure-skip-verify", false, "skip TLS verify on outbound (dev only)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *showVer {
		_, err := fmt.Fprintln(stdout, version.Version)
		return err
	}

	tr := p2p.NewTCPTransport(*listen)
	tr.Logger = log
	tr.MaxPeers = *maxPeers
	tr.DialTimeout = *dialTimeout
	tr.ReadIdleTimeout = *readIdle
	tr.OnPeer = func(peer p2p.Peer) {
		log.Info("peer", "remote", peer.RemoteAddr().String(), "outbound", peer.IsOutbound())
	}

	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return fmt.Errorf("tls: both -tls-cert and -tls-key are required")
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("tls: load server cert: %w", err)
		}
		tr.TLSServerConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	if *dial != "" && (*tlsCA != "" || *insecureSkip || *tlsServerName != "") {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if *insecureSkip {
			cfg.InsecureSkipVerify = true
		}
		if *tlsServerName != "" {
			cfg.ServerName = *tlsServerName
		}
		if *tlsCA != "" {
			pool := x509.NewCertPool()
			data, err := os.ReadFile(*tlsCA)
			if err != nil {
				return fmt.Errorf("tls: read ca: %w", err)
			}
			if !pool.AppendCertsFromPEM(data) {
				return fmt.Errorf("tls: no certificates parsed from -tls-ca")
			}
			cfg.RootCAs = pool
		}
		tr.TLSClientConfig = cfg
	}

	if err := tr.ListenAndAccept(ctx); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		_ = tr.Close()
	}()

	addr := tr.Addr()
	if addr == nil {
		return errors.New("no listen address")
	}
	if _, err := fmt.Fprintf(stdout, "listening on %s\n", addr.String()); err != nil {
		return err
	}

	if *dial != "" {
		if err := tr.Dial(ctx, *dial); err != nil {
			return fmt.Errorf("dial: %w", err)
		}
	}

	var hsrv *http.Server
	if *health != "" {
		var err error
		hsrv, err = startHealth(*health, tr, log)
		if err != nil {
			return fmt.Errorf("health: %w", err)
		}
		defer func() {
			shctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = hsrv.Shutdown(shctx)
		}()
	}

	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

func startHealth(addr string, tr *p2p.TCPTransport, log *slog.Logger) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !tr.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(tr.Metrics().Snapshot())
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("health", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
		}
	}()
	return srv, nil
}
