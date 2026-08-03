package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AliSinaDevelo/StreamHive/internal/version"
	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
)

const (
	defaultPutAckTimeout = time.Second
	defaultPutRetries    = 2
	defaultPutRetryDelay = 100 * time.Millisecond
	maxPutRetries        = 10
	maxPutRetryDelay     = 500 * time.Millisecond
	maxDuration          = time.Duration(1<<63 - 1)
)

var errBlobAckTimeout = errors.New("replication: timed out waiting for blob acknowledgment")

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
	peers := fs.String("peers", "", "comma-separated peer host:port list to dial after listen")
	peerReconnect := fs.Bool("peer-reconnect", false, "keep retrying -peers with exponential backoff")
	peerReconnectMin := fs.Duration("peer-reconnect-min", 500*time.Millisecond, "minimum reconnect backoff for -peer-reconnect")
	peerReconnectMax := fs.Duration("peer-reconnect-max", 30*time.Second, "maximum reconnect backoff for -peer-reconnect")
	syncInterval := fs.Duration("sync-interval", 0, "periodically advertise local blob keys to connected peers (0 = startup only)")
	health := fs.String("health", "", "optional HTTP listen addr for /livez /readyz /peers /metrics (e.g. :8080)")
	maxPeers := fs.Int("max-peers", 0, "max simultaneous peers (0 = unlimited)")
	peerAuthToken := fs.String("peer-auth-token", "", "optional shared token required before peer registration")
	peerAuthTimeout := fs.Duration("peer-auth-timeout", p2p.DefaultPeerAuthTimeout, "timeout for optional peer auth handshake")
	dialTimeout := fs.Duration("dial-timeout", 0, "default dial timeout (0 = use context only)")
	readIdle := fs.Duration("read-idle-timeout", 0, "TCP read deadline refresh for peer loops (0 = none for discard mode)")
	showVer := fs.Bool("version", false, "print version and exit")
	replicate := fs.Bool("replicate", false, "enable in-memory blob replication from framed peers")
	storeDir := fs.String("store-dir", "", "directory for durable replicated blobs (requires -replicate)")
	listKeys := fs.Bool("list-keys", false, "print durable store keys as hex and exit (requires -store-dir)")
	putKey := fs.String("put-key", "", "send one replicated blob key to -dial peer")
	putData := fs.String("put-data", "", "send one replicated blob value to -dial peer")
	putContentKey := fs.Bool("put-content-key", false, "derive the replicated blob key from SHA-256(-put-data)")
	exitAfterPut := fs.Bool("exit-after-put", false, "exit after sending one blob to outbound peers")
	putAckTimeout := fs.Duration("put-ack-timeout", defaultPutAckTimeout, "time to wait for each blob acknowledgment")
	putRetries := fs.Int("put-retries", defaultPutRetries, "additional blob sends after an acknowledgment timeout")
	putRetryDelay := fs.Duration("put-retry-delay", defaultPutRetryDelay, "delay before retrying a blob after an acknowledgment timeout")
	maxBlobBytes := fs.Int("max-blob-bytes", replication.DefaultMaxDataBytes, "max replicated blob payload bytes")

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
	dialTarget := strings.TrimSpace(*dial)
	peerList, err := parsePeerList(*peers)
	if err != nil {
		return err
	}
	peerTargets := combinePeerTargets(dialTarget, peerList)
	putRequested := *putKey != "" || *putContentKey
	if putRequested && len(peerTargets) == 0 {
		return fmt.Errorf("replication: -put-key or -put-content-key requires -dial or -peers")
	}
	if *putContentKey && *putKey != "" {
		return fmt.Errorf("replication: -put-content-key cannot be combined with -put-key")
	}
	if *peerReconnect {
		if len(peerList) == 0 {
			return fmt.Errorf("peers: -peer-reconnect requires -peers")
		}
		if putRequested {
			return fmt.Errorf("replication: -peer-reconnect cannot be combined with -put-key or -put-content-key")
		}
		if err := validateReconnectBackoff(*peerReconnectMin, *peerReconnectMax); err != nil {
			return err
		}
	}
	if *listKeys {
		if *storeDir == "" {
			return fmt.Errorf("storage: -list-keys requires -store-dir")
		}
		store, err := storage.NewFileStore(*storeDir)
		if err != nil {
			return fmt.Errorf("storage: open file store: %w", err)
		}
		keys, err := store.ListKeys(ctx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if _, err := fmt.Fprintln(stdout, hex.EncodeToString(key)); err != nil {
				return err
			}
		}
		return nil
	}
	if *storeDir != "" && !*replicate {
		return fmt.Errorf("storage: -store-dir requires -replicate")
	}
	if *syncInterval < 0 {
		return fmt.Errorf("replication: -sync-interval must be zero or greater")
	}
	if *peerAuthTimeout < 0 {
		return fmt.Errorf("peers: -peer-auth-timeout must be zero or greater")
	}
	if *putAckTimeout <= 0 {
		return fmt.Errorf("replication: -put-ack-timeout must be greater than zero")
	}
	if *putRetries < 0 || *putRetries > maxPutRetries {
		return fmt.Errorf("replication: -put-retries must be between 0 and %d", maxPutRetries)
	}
	if *putRetryDelay < 0 {
		return fmt.Errorf("replication: -put-retry-delay must be zero or greater")
	}

	replLimits := replication.Limits{MaxDataBytes: *maxBlobBytes}
	var blobStore storage.BlobStore
	var keyLister storage.BlobKeyLister
	var memoryStore *storage.MemoryStore
	replMetrics := &replicationMetrics{}
	if *replicate {
		if *storeDir != "" {
			var err error
			blobStore, err = storage.NewFileStore(*storeDir)
			if err != nil {
				return fmt.Errorf("storage: open file store: %w", err)
			}
		} else {
			memoryStore = storage.NewMemoryStore()
			blobStore = memoryStore
		}
		if lister, ok := blobStore.(storage.BlobKeyLister); ok {
			keyLister = lister
		}
	}
	var putPayload []byte
	var putKeyBytes []byte
	var putKeyLabel string
	var putResult chan error
	var putTracker *putAckTracker
	if putRequested {
		putKeyBytes, putKeyLabel = resolvePutKey(*putKey, []byte(*putData), *putContentKey)
		putPayload, err = replication.EncodeBlobPut(putKeyBytes, []byte(*putData), replLimits)
		if err != nil {
			return err
		}
		putResult = make(chan error, len(peerTargets))
		putTracker = newPutAckTracker(replMetrics)
		replMetrics.ackTracker = putTracker
	}

	tr := p2p.NewTCPTransport(*listen)
	tr.Logger = log
	tr.MaxPeers = *maxPeers
	tr.PeerAuthToken = *peerAuthToken
	tr.PeerAuthTimeout = *peerAuthTimeout
	tr.DialTimeout = *dialTimeout
	tr.ReadIdleTimeout = *readIdle
	tr.OnPeer = func(peer p2p.Peer) {
		log.Info("peer", "remote", peer.RemoteAddr().String(), "outbound", peer.IsOutbound(), "auth_method", authMethodForPeer(peer))
		if putPayload != nil && peer.IsOutbound() {
			go func() {
				err := sendBlobWithAck(
					ctx,
					peer,
					putPayload,
					putKeyBytes,
					len(*putData),
					tr.MaxFrameBytes,
					putTracker,
					*putAckTimeout,
					*putRetries,
					*putRetryDelay,
					replMetrics,
					log,
				)
				if err != nil {
					log.Error("replication send", "remote", peer.RemoteAddr().String(), "key", putKeyLabel, "err", err)
				} else {
					log.Info("replicated blob accepted", "remote", peer.RemoteAddr().String(), "key", putKeyLabel, "bytes", len(*putData))
				}
				reportPutResult(putResult, err)
			}()
		}
		if keyLister != nil {
			if err := sendBlobHas(ctx, peer, keyLister, replLimits, tr.MaxFrameBytes); err != nil {
				replMetrics.SendErrors.Add(1)
				log.Error("replication inventory send", "remote", peer.RemoteAddr().String(), "err", err)
				_ = peer.Close()
			}
		}
	}
	if blobStore != nil || putTracker != nil {
		tr.FrameHandler = func(ctx context.Context, peer p2p.Peer, payload []byte) error {
			msg, err := replication.Decode(payload, replLimits)
			if err != nil {
				replMetrics.ApplyErrors.Add(1)
				return err
			}
			if blobStore == nil && msg.Type != replication.MessageTypeBlobAck {
				return nil
			}
			if err := handleReplicationMessage(ctx, peer, blobStore, keyLister, msg, replLimits, tr.MaxFrameBytes, replMetrics, log, memoryStore); err != nil {
				replMetrics.ApplyErrors.Add(1)
				return err
			}
			return nil
		}
	}
	var reconnector *peerReconnector
	if *peerReconnect {
		reconnector = newPeerReconnector(ctx, tr, peerList, *peerReconnectMin, *peerReconnectMax, log)
		tr.OnPeerDisconnected = reconnector.OnPeerDisconnected
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

	if len(peerTargets) > 0 && (*tlsCA != "" || *insecureSkip || *tlsServerName != "") {
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
	if keyLister != nil && *syncInterval > 0 {
		startPeriodicBlobHas(ctx, tr, keyLister, replLimits, *syncInterval, replMetrics, log)
	}

	addr := tr.Addr()
	if addr == nil {
		return errors.New("no listen address")
	}
	if _, err := fmt.Fprintf(stdout, "listening on %s\n", addr.String()); err != nil {
		return err
	}

	if dialTarget != "" {
		if err := tr.Dial(ctx, dialTarget); err != nil {
			return fmt.Errorf("dial %s: %w", dialTarget, err)
		}
	}
	if reconnector != nil {
		reconnector.Start()
	} else {
		for _, target := range peerList {
			if err := tr.Dial(ctx, target); err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
		}
	}
	if *exitAfterPut && putResult != nil {
		waitTimeout := putWaitTimeout(*putAckTimeout, *putRetryDelay, *putRetries)
		for range peerTargets {
			select {
			case err := <-putResult:
				if err != nil {
					return fmt.Errorf("replication: send blob: %w", err)
				}
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				return ctx.Err()
			case <-time.After(waitTimeout):
				return fmt.Errorf("replication: timed out waiting for blob send")
			}
		}
		return nil
	}

	var hsrv *http.Server
	if *health != "" {
		var err error
		hsrv, err = startHealth(*health, tr, replMetrics, log)
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

func reportPutResult(ch chan<- error, err error) {
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

type peerAuthMethodProvider interface {
	AuthMethod() string
}

func authMethodForPeer(peer p2p.Peer) string {
	if provider, ok := peer.(peerAuthMethodProvider); ok {
		return provider.AuthMethod()
	}
	return p2p.PeerAuthMethodNone
}

type putAckID struct {
	remote string
	key    string
}

type putAckTracker struct {
	mu      sync.Mutex
	pending map[putAckID]chan struct{}
	metrics *replicationMetrics
}

func newPutAckTracker(metrics *replicationMetrics) *putAckTracker {
	return &putAckTracker{
		pending: make(map[putAckID]chan struct{}),
		metrics: metrics,
	}
}

func (t *putAckTracker) register(peer p2p.Peer, key []byte) <-chan struct{} {
	ack := make(chan struct{})
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	t.pending[id] = ack
	if t.metrics != nil {
		t.metrics.BlobAcksPending.Add(1)
	}
	t.mu.Unlock()
	return ack
}

func (t *putAckTracker) ack(peer p2p.Peer, key []byte) bool {
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	ack, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
		if t.metrics != nil {
			t.metrics.BlobAcksPending.Add(-1)
			t.metrics.BlobAcksMatched.Add(1)
		}
		close(ack)
	}
	t.mu.Unlock()
	return ok
}

func (t *putAckTracker) remove(peer p2p.Peer, key []byte) bool {
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	_, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
		if t.metrics != nil {
			t.metrics.BlobAcksPending.Add(-1)
		}
	}
	t.mu.Unlock()
	return ok
}

func sendBlobWithAck(
	ctx context.Context,
	peer p2p.Peer,
	payload []byte,
	key []byte,
	blobBytes int,
	maxFrameBytes int,
	tracker *putAckTracker,
	ackTimeout time.Duration,
	retries int,
	retryDelay time.Duration,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	if tracker == nil {
		return errors.New("replication: blob acknowledgment tracker is required")
	}
	for attempt := 0; attempt <= retries; attempt++ {
		ack := tracker.register(peer, key)
		if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
			tracker.remove(peer, key)
			metrics.SendErrors.Add(1)
			return fmt.Errorf("replication: write blob: %w", err)
		}
		metrics.BlobsSent.Add(1)
		metrics.BytesSent.Add(uint64(blobBytes))
		log.Info("replicated blob sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "bytes", blobBytes, "attempt", attempt+1)

		timer := time.NewTimer(ackTimeout)
		select {
		case <-ack:
			timer.Stop()
			return nil
		case <-ctx.Done():
			timer.Stop()
			tracker.remove(peer, key)
			return ctx.Err()
		case <-timer.C:
			if !tracker.remove(peer, key) {
				return nil
			}
			metrics.BlobAckTimeouts.Add(1)
			if attempt == retries {
				return errBlobAckTimeout
			}
			metrics.BlobRetries.Add(1)
			delay := retryDelayForAttempt(retryDelay, attempt)
			log.Warn("replication blob ack timeout", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "retry", attempt+1, "delay", delay)
			if !sleepContext(ctx, delay) {
				return ctx.Err()
			}
		}
	}
	return errBlobAckTimeout
}

func retryDelayForAttempt(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	if base >= maxPutRetryDelay {
		return maxPutRetryDelay
	}
	delay := base
	for i := 0; i < attempt; i++ {
		if delay >= maxPutRetryDelay/2 {
			return maxPutRetryDelay
		}
		delay *= 2
	}
	if delay > maxPutRetryDelay {
		return maxPutRetryDelay
	}
	return delay
}

func putWaitTimeout(ackTimeout, retryDelay time.Duration, retries int) time.Duration {
	total := time.Second
	for range retries + 1 {
		total = addDuration(total, ackTimeout)
	}
	for attempt := 0; attempt < retries; attempt++ {
		total = addDuration(total, retryDelayForAttempt(retryDelay, attempt))
	}
	return total
}

func addDuration(left, right time.Duration) time.Duration {
	if right > 0 && left > maxDuration-right {
		return maxDuration
	}
	return left + right
}

func handleReplicationMessage(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	lister storage.BlobKeyLister,
	msg replication.Message,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
	memoryStore *storage.MemoryStore,
) error {
	switch msg.Type {
	case replication.MessageTypeBlobPut:
		if err := verifyContentKeyIfSHA256(msg.Key, msg.Data); err != nil {
			return err
		}
		existing, err := store.Get(ctx, msg.Key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && bytes.Equal(existing, msg.Data) {
			metrics.DuplicateBlobs.Add(1)
			metrics.DuplicateBytes.Add(uint64(len(msg.Data)))
			log.Info("replicated blob duplicate", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "bytes", len(msg.Data))
			return sendBlobAck(peer, msg.Key, limits, maxFrameBytes, metrics, log)
		}
		if err := store.Put(ctx, msg.Key, msg.Data); err != nil {
			return err
		}
		metrics.BlobsStored.Add(1)
		metrics.BytesStored.Add(uint64(len(msg.Data)))
		attrs := []any{"remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "bytes", len(msg.Data)}
		if memoryStore != nil {
			attrs = append(attrs, "blobs", memoryStore.Len())
		}
		log.Info("replicated blob stored", attrs...)
		return sendBlobAck(peer, msg.Key, limits, maxFrameBytes, metrics, log)
	case replication.MessageTypeBlobHas:
		if lister == nil {
			return nil
		}
		localKeys, err := lister.ListKeys(ctx)
		if err != nil {
			return err
		}
		missing := missingKeys(msg.Keys, localKeys)
		if len(missing) == 0 {
			return nil
		}
		payload, err := replication.EncodeBlobMissing(missing, limits)
		if err != nil {
			return err
		}
		if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
			metrics.SendErrors.Add(1)
			return err
		}
		log.Info("replication missing sent", "remote", peer.RemoteAddr().String(), "keys", len(missing))
		return nil
	case replication.MessageTypeBlobMissing:
		return sendRequestedBlobs(ctx, peer, store, msg.Keys, limits, maxFrameBytes, metrics, log)
	case replication.MessageTypeBlobGet:
		return sendRequestedBlobs(ctx, peer, store, [][]byte{msg.Key}, limits, maxFrameBytes, metrics, log)
	case replication.MessageTypeBlobAck:
		metrics.BlobAcksReceived.Add(1)
		matched := false
		if metrics.ackTracker != nil {
			matched = metrics.ackTracker.ack(peer, msg.Key)
		}
		log.Info("replication ack received", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "matched", matched)
		return nil
	default:
		return replication.ErrUnknownMessageType
	}
}

func sendBlobAck(
	peer p2p.Peer,
	key []byte,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	payload, err := replication.EncodeBlobAck(key, limits)
	if err != nil {
		metrics.SendErrors.Add(1)
		return err
	}
	if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
		metrics.SendErrors.Add(1)
		return err
	}
	metrics.BlobAcksSent.Add(1)
	log.Info("replication ack sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key))
	return nil
}

func sendBlobHas(ctx context.Context, peer p2p.Peer, lister storage.BlobKeyLister, limits replication.Limits, maxFrameBytes int) error {
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	payload, err := replication.EncodeBlobHas(keys, limits)
	if err != nil {
		return err
	}
	return writePeerFrame(peer, payload, maxFrameBytes)
}

func startPeriodicBlobHas(
	ctx context.Context,
	tr *p2p.TCPTransport,
	lister storage.BlobKeyLister,
	limits replication.Limits,
	interval time.Duration,
	metrics *replicationMetrics,
	log *slog.Logger,
) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for _, peer := range tr.Peers() {
				if err := sendBlobHas(ctx, peer, lister, limits, tr.MaxFrameBytes); err != nil {
					metrics.SendErrors.Add(1)
					log.Error("replication periodic inventory send", "remote", peer.RemoteAddr().String(), "err", err)
				}
			}
		}
	}()
}

func sendRequestedBlobs(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	keys [][]byte,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	for _, key := range keys {
		data, err := store.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			metrics.SendErrors.Add(1)
			metrics.BlobsSkipped.Add(1)
			log.Warn("replicated blob skipped", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "err", err)
			continue
		}
		payload, err := replication.EncodeBlobPut(key, data, limits)
		if err != nil {
			metrics.SendErrors.Add(1)
			metrics.BlobsSkipped.Add(1)
			log.Warn("replicated blob skipped", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "err", err)
			continue
		}
		if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
			metrics.SendErrors.Add(1)
			return err
		}
		metrics.BlobsSent.Add(1)
		metrics.BytesSent.Add(uint64(len(data)))
		log.Info("replicated blob sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "bytes", len(data))
	}
	return nil
}

func resolvePutKey(key string, data []byte, contentKey bool) ([]byte, string) {
	if contentKey {
		digest := storage.SHA256Key(data)
		return digest, storage.SHA256KeyHex(data)
	}
	return []byte(key), key
}

func formatBlobKey(key []byte) string {
	if err := storage.ValidateSHA256Key(key); err == nil {
		return hex.EncodeToString(key)
	}
	return string(key)
}

func verifyContentKeyIfSHA256(key, data []byte) error {
	if err := storage.ValidateSHA256Key(key); err != nil {
		return nil
	}
	return storage.VerifySHA256Key(key, data)
}

type frameWriter interface {
	WriteFrame(payload []byte, maxPayload int) error
}

func writePeerFrame(peer p2p.Peer, payload []byte, maxFrameBytes int) error {
	writer, ok := peer.(frameWriter)
	if !ok {
		return errors.New("peer cannot write frames")
	}
	return writer.WriteFrame(payload, maxFrameBytes)
}

func missingKeys(remoteKeys, localKeys [][]byte) [][]byte {
	local := make(map[string]struct{}, len(localKeys))
	for _, key := range localKeys {
		local[string(key)] = struct{}{}
	}
	missing := make([][]byte, 0)
	for _, key := range remoteKeys {
		if _, ok := local[string(key)]; ok {
			continue
		}
		missing = append(missing, append([]byte(nil), key...))
	}
	return missing
}

type peerReconnector struct {
	ctx    context.Context
	tr     *p2p.TCPTransport
	min    time.Duration
	max    time.Duration
	log    *slog.Logger
	target map[string]struct{}

	mu      sync.Mutex
	dialing map[string]bool
}

func newPeerReconnector(ctx context.Context, tr *p2p.TCPTransport, targets []string, minBackoff, maxBackoff time.Duration, log *slog.Logger) *peerReconnector {
	targetMap := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetMap[target] = struct{}{}
	}
	return &peerReconnector{
		ctx:     ctx,
		tr:      tr,
		min:     minBackoff,
		max:     maxBackoff,
		log:     log,
		target:  targetMap,
		dialing: make(map[string]bool, len(targets)),
	}
}

func (r *peerReconnector) Start() {
	for target := range r.target {
		r.schedule(target, 0)
	}
}

func (r *peerReconnector) OnPeerDisconnected(peer p2p.Peer) {
	if !peer.IsOutbound() {
		return
	}
	target := peer.RemoteAddr().String()
	if _, ok := r.target[target]; !ok {
		return
	}
	r.schedule(target, r.min)
}

func (r *peerReconnector) schedule(target string, initialDelay time.Duration) {
	r.mu.Lock()
	if r.dialing[target] {
		r.mu.Unlock()
		return
	}
	r.dialing[target] = true
	r.mu.Unlock()

	go r.loop(target, initialDelay)
}

func (r *peerReconnector) loop(target string, initialDelay time.Duration) {
	defer func() {
		r.mu.Lock()
		delete(r.dialing, target)
		r.mu.Unlock()
	}()

	delay := r.min
	if initialDelay > 0 {
		if !sleepContext(r.ctx, initialDelay) {
			return
		}
	}

	for {
		if err := r.ctx.Err(); err != nil {
			return
		}
		err := r.tr.Dial(r.ctx, target)
		if err == nil {
			r.log.Info("peer reconnect established", "target", target)
			return
		}
		r.log.Warn("peer reconnect failed", "target", target, "err", err, "next", delay)
		if !sleepContext(r.ctx, delay) {
			return
		}
		delay = nextBackoff(delay, r.max)
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maxDelay time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > maxDelay {
		return maxDelay
	}
	return next
}

func parsePeerTargets(dial, peers string) ([]string, error) {
	peerList, err := parsePeerList(peers)
	if err != nil {
		return nil, err
	}
	return combinePeerTargets(strings.TrimSpace(dial), peerList), nil
}

func parsePeerList(peers string) ([]string, error) {
	if strings.TrimSpace(peers) == "" {
		return nil, nil
	}
	targets := make([]string, 0, 1)
	for _, part := range strings.Split(peers, ",") {
		target := strings.TrimSpace(part)
		if target == "" {
			return nil, fmt.Errorf("peers: empty peer address")
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func combinePeerTargets(dial string, peers []string) []string {
	targets := make([]string, 0, len(peers)+1)
	if dial != "" {
		targets = append(targets, dial)
	}
	targets = append(targets, peers...)
	return targets
}

func validateReconnectBackoff(minBackoff, maxBackoff time.Duration) error {
	if minBackoff <= 0 {
		return fmt.Errorf("peers: -peer-reconnect-min must be greater than zero")
	}
	if maxBackoff < minBackoff {
		return fmt.Errorf("peers: -peer-reconnect-max must be greater than or equal to -peer-reconnect-min")
	}
	return nil
}

type replicationMetrics struct {
	BlobsStored      atomic.Uint64
	BytesStored      atomic.Uint64
	DuplicateBlobs   atomic.Uint64
	DuplicateBytes   atomic.Uint64
	ApplyErrors      atomic.Uint64
	BlobsSent        atomic.Uint64
	BytesSent        atomic.Uint64
	BlobsSkipped     atomic.Uint64
	BlobAcksSent     atomic.Uint64
	BlobAcksReceived atomic.Uint64
	BlobAcksMatched  atomic.Uint64
	BlobAcksPending  atomic.Int64
	BlobAckTimeouts  atomic.Uint64
	BlobRetries      atomic.Uint64
	SendErrors       atomic.Uint64
	ackTracker       *putAckTracker
}

func (m *replicationMetrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"replication_blobs_stored":       int64(m.BlobsStored.Load()),
		"replication_bytes_stored":       int64(m.BytesStored.Load()),
		"replication_duplicate_blobs":    int64(m.DuplicateBlobs.Load()),
		"replication_duplicate_bytes":    int64(m.DuplicateBytes.Load()),
		"replication_apply_errors":       int64(m.ApplyErrors.Load()),
		"replication_blobs_sent":         int64(m.BlobsSent.Load()),
		"replication_bytes_sent":         int64(m.BytesSent.Load()),
		"replication_blobs_skipped":      int64(m.BlobsSkipped.Load()),
		"replication_blob_acks_sent":     int64(m.BlobAcksSent.Load()),
		"replication_blob_acks_received": int64(m.BlobAcksReceived.Load()),
		"replication_blob_acks_matched":  int64(m.BlobAcksMatched.Load()),
		"replication_blob_acks_pending":  m.BlobAcksPending.Load(),
		"replication_blob_ack_timeouts":  int64(m.BlobAckTimeouts.Load()),
		"replication_blob_retries":       int64(m.BlobRetries.Load()),
		"replication_send_errors":        int64(m.SendErrors.Load()),
	}
}

type peerStatus struct {
	RemoteAddr     string `json:"remote_addr"`
	LocalAddr      string `json:"local_addr,omitempty"`
	Outbound       bool   `json:"outbound"`
	ConnectedAt    string `json:"connected_at,omitempty"`
	ConnectedForMS int64  `json:"connected_for_ms"`
	AuthMethod     string `json:"auth_method"`
}

type peersResponse struct {
	ActivePeers int          `json:"active_peers"`
	Peers       []peerStatus `json:"peers"`
}

func snapshotPeers(peers []p2p.PeerSnapshot, now time.Time) peersResponse {
	statuses := make([]peerStatus, 0, len(peers))
	for _, peer := range peers {
		connectedAt := ""
		var connectedForMS int64
		if !peer.ConnectedAt.IsZero() {
			connectedAt = peer.ConnectedAt.UTC().Format(time.RFC3339Nano)
			if now.After(peer.ConnectedAt) {
				connectedForMS = now.Sub(peer.ConnectedAt).Milliseconds()
			}
		}
		statuses = append(statuses, peerStatus{
			RemoteAddr:     peer.RemoteAddr,
			LocalAddr:      peer.LocalAddr,
			Outbound:       peer.Outbound,
			ConnectedAt:    connectedAt,
			ConnectedForMS: connectedForMS,
			AuthMethod:     peer.AuthMethod,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].RemoteAddr == statuses[j].RemoteAddr {
			return !statuses[i].Outbound && statuses[j].Outbound
		}
		return statuses[i].RemoteAddr < statuses[j].RemoteAddr
	})
	return peersResponse{
		ActivePeers: len(statuses),
		Peers:       statuses,
	}
}

func startHealth(addr string, tr *p2p.TCPTransport, replMetrics *replicationMetrics, log *slog.Logger) (*http.Server, error) {
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
		snapshot := tr.Metrics().Snapshot()
		for key, value := range replMetrics.Snapshot() {
			snapshot[key] = value
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snapshot)
	})
	mux.HandleFunc("/peers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snapshotPeers(tr.PeerSnapshots(), time.Now()))
	})
	mux.HandleFunc("/metrics/prometheus", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		snapshot := tr.Metrics().Snapshot()
		for key, value := range replMetrics.Snapshot() {
			snapshot[key] = value
		}
		writePrometheusMetrics(w, snapshot)
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

func writePrometheusMetrics(w io.Writer, snapshot map[string]int64) {
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = fmt.Fprintf(w, "streamhive_%s %d\n", key, snapshot[key])
	}
}
