package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// safeBuffer is an io.Writer safe for concurrent writes and reads from another goroutine (e.g. with require.Eventually).
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

type testPeer struct{}

func (testPeer) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7070} }
func (testPeer) Close() error         { return nil }
func (testPeer) IsOutbound() bool     { return false }

type capturePeer struct {
	testPeer
	err      error
	payloads [][]byte
}

type asyncCapturePeer struct {
	testPeer
	addr           net.Addr
	writeStarted   chan struct{}
	writeRelease   <-chan struct{}
	startWriteOnce sync.Once
	mu             sync.Mutex
	payloads       [][]byte
}

type hasProbeStore struct {
	storage.BlobStore
	hasCalls atomic.Int32
}

func (s *hasProbeStore) Has(ctx context.Context, key []byte) (bool, error) {
	s.hasCalls.Add(1)
	return s.BlobStore.Has(ctx, key)
}

type cancelAfterGetStore struct {
	storage.BlobStore
	cancel context.CancelFunc
}

func (s *cancelAfterGetStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	data, err := s.BlobStore.Get(ctx, key)
	s.cancel()
	return data, err
}

func (p *capturePeer) WriteFrame(payload []byte, _ int) error {
	if p.err != nil {
		return p.err
	}
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
	return nil
}

func (p *asyncCapturePeer) WriteFrame(payload []byte, _ int) error {
	if p.writeStarted != nil {
		p.startWriteOnce.Do(func() { close(p.writeStarted) })
	}
	if p.writeRelease != nil {
		<-p.writeRelease
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
	return nil
}

func (p *asyncCapturePeer) RemoteAddr() net.Addr {
	if p.addr != nil {
		return p.addr
	}
	return p.testPeer.RemoteAddr()
}

func (p *asyncCapturePeer) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.payloads)
}

func (p *asyncCapturePeer) Payloads() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	payloads := make([][]byte, len(p.payloads))
	for i, payload := range p.payloads {
		payloads[i] = append([]byte(nil), payload...)
	}
	return payloads
}

type retryPeer struct {
	testPeer
	writes      atomic.Int32
	writeEvents chan struct{}
}

func (p *retryPeer) WriteFrame(_ []byte, _ int) error {
	p.writes.Add(1)
	p.writeEvents <- struct{}{}
	return nil
}

type writeFailurePeer struct {
	testPeer
	err    error
	closes atomic.Int32
}

func (p *writeFailurePeer) WriteFrame([]byte, int) error { return p.err }
func (p *writeFailurePeer) Close() error {
	p.closes.Add(1)
	return nil
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRun_version(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-version"}, &out, io.Discard)
	require.NoError(t, err)
	assert.NotEmpty(t, strings.TrimSpace(out.String()))
}

func TestRun_listenUntilCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{"-listen", "127.0.0.1:0"}, &out, io.Discard)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "listening on")
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestRun_healthEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, stderr safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{"-listen", "127.0.0.1:0", "-health", "127.0.0.1:0"}, &out, &stderr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "listening on") &&
			strings.Contains(stderr.String(), "addr=")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`addr=([0-9a-fA-F.:]+)`)
	m := re.FindStringSubmatch(stderr.String())
	require.Len(t, m, 2, "stderr=%q", stderr.String())

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + m[1]

	resp, err := client.Get(base + "/livez")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp2, err := client.Get(base + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	resp3, err := client.Get(base + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	var metrics map[string]int64
	require.NoError(t, json.NewDecoder(resp3.Body).Decode(&metrics))
	assert.Contains(t, metrics, "active_peers")
	assert.Contains(t, metrics, "replication_blobs_stored")
	assert.Contains(t, metrics, "replication_blob_puts_accepted")
	assert.Contains(t, metrics, "replication_blob_put_failures")
	assert.Contains(t, metrics, "replication_blob_write_errors")
	assert.Contains(t, metrics, "replication_inventory_advertisements")
	assert.Contains(t, metrics, "replication_missing_keys_requested")
	assert.Contains(t, metrics, "replication_repair_blobs_sent")
	assert.Contains(t, metrics, "replication_repair_blobs_deferred")
	assert.Contains(t, metrics, "replication_corrupt_blobs_detected")
	assert.Contains(t, metrics, "replication_repair_continuations_scheduled")
	assert.Contains(t, metrics, "replication_repair_continuations_completed")
	assert.Contains(t, metrics, "replication_repair_continuations_dropped")
	assert.Contains(t, metrics, "replication_repair_continuations_active")
	assert.Contains(t, metrics, "replication_repair_continuation_keys_pending")
	assert.Zero(t, metrics["replication_repair_continuations_scheduled"])
	assert.Zero(t, metrics["replication_repair_continuations_completed"])
	assert.Zero(t, metrics["replication_repair_continuations_dropped"])
	assert.Zero(t, metrics["replication_corrupt_blobs_detected"])
	assert.Zero(t, metrics["replication_repair_continuations_active"])
	assert.Zero(t, metrics["replication_repair_continuation_keys_pending"])
	assert.Contains(t, metrics, "peer_auth_identity_rejections")

	resp4, err := client.Get(base + "/metrics/prometheus")
	require.NoError(t, err)
	defer func() { _ = resp4.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp4.StatusCode)
	body, err := io.ReadAll(resp4.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "streamhive_active_peers")
	assert.Contains(t, string(body), "streamhive_replication_blobs_stored")
	assert.Contains(t, string(body), "streamhive_replication_blob_puts_accepted")
	assert.Contains(t, string(body), "streamhive_replication_inventory_advertisements")
	assert.Contains(t, string(body), "streamhive_replication_missing_keys_requested")
	assert.Contains(t, string(body), "streamhive_replication_repair_blobs_sent")
	assert.Contains(t, string(body), "streamhive_replication_repair_blobs_deferred")
	assert.Contains(t, string(body), "streamhive_replication_corrupt_blobs_detected")
	assert.Contains(t, string(body), "streamhive_replication_repair_continuations_scheduled")
	assert.Contains(t, string(body), "streamhive_replication_repair_continuations_completed")
	assert.Contains(t, string(body), "streamhive_replication_repair_continuations_dropped")
	assert.Contains(t, string(body), "streamhive_replication_repair_continuations_active")
	assert.Contains(t, string(body), "streamhive_replication_repair_continuation_keys_pending")
	assert.Contains(t, string(body), "streamhive_peer_auth_identity_rejections")

	resp5, err := client.Get(base + "/peers")
	require.NoError(t, err)
	defer func() { _ = resp5.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp5.StatusCode)
	var peers peersResponse
	require.NoError(t, json.NewDecoder(resp5.Body).Decode(&peers))
	assert.Equal(t, 0, peers.ActivePeers)
	assert.Empty(t, peers.Peers)

	cancel()
	<-errCh
}

func TestRun_putRequiresDial(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-put-key", "alpha", "-put-data", "hello"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-put-key or -put-content-key requires -dial or -peers")
}

func TestRun_putContentKeyRejectsExplicitKey(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{
		"-dial", "127.0.0.1:1",
		"-put-key", "alpha",
		"-put-content-key",
		"-put-data", "hello",
	}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-put-content-key cannot be combined with -put-key")
}

func TestRun_storeDirRequiresReplicate(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-store-dir", t.TempDir()}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-store-dir requires -replicate")
}

func TestRun_listKeysRequiresStoreDir(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-list-keys"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-list-keys requires -store-dir")
}

func TestRun_listKeysPrintsDurableKeysAsHex(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, []byte("b"), []byte("second")))
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("first")))

	var out bytes.Buffer
	err = run(ctx, []string{"-store-dir", storeDir, "-list-keys"}, &out, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "61\n62\n", out.String())
}

func TestRun_peerReconnectRequiresPeers(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-peer-reconnect"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-peer-reconnect requires -peers")
}

func TestRun_peerReconnectRejectsOneShotPut(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{
		"-peers", "127.0.0.1:1",
		"-peer-reconnect",
		"-put-key", "alpha",
		"-put-data", "hello",
	}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with -put-key")
}

func TestRun_peerIDRequiresAuthToken(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-peer-id", "node-a"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-peer-id requires -peer-auth-token")
}

func TestRun_peerAllowIDsRequiresAuthToken(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-peer-allow-ids", "node-a"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-peer-allow-ids requires -peer-auth-token")
}

func TestRun_syncIntervalRejectsNegative(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-sync-interval", "-1s"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-sync-interval must be zero or greater")
}

func TestRun_maxRepairBytesRejectsNegative(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-max-repair-bytes", "-1"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-max-repair-bytes must be zero or greater")
}

func TestRun_replicatesBlobPutToDialPeer(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{"-listen", "127.0.0.1:0", "-replicate"}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(serverOut.String())
	require.Len(t, m, 2, "stdout=%q", serverOut.String())

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	var clientOut, clientErr safeBuffer
	clientErrCh := make(chan error, 1)
	go func() {
		clientErrCh <- run(clientCtx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", m[1],
			"-put-key", "alpha",
			"-put-data", "hello",
			"-exit-after-put",
		}, &clientOut, &clientErr)
	}()

	require.Eventually(t, func() bool {
		logs := serverErr.String()
		return strings.Contains(logs, "replicated blob stored") &&
			strings.Contains(logs, "key=alpha") &&
			strings.Contains(logs, "bytes=5")
	}, 3*time.Second, 20*time.Millisecond, "server logs=%q client logs=%q", serverErr.String(), clientErr.String())

	serverCancel()
	require.NoError(t, <-clientErrCh)
	require.NoError(t, <-serverErrCh)
}

func TestRun_peerIdentityAppearsInConnectionLogs(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-peer-auth-token", "shared-secret",
			"-peer-id", "node-a",
			"-peer-allow-ids", "node-b",
		}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)
	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(serverOut.String())
	require.Len(t, m, 2, "stdout=%q", serverOut.String())

	var clientOut, clientErr safeBuffer
	err := run(context.Background(), []string{
		"-listen", "127.0.0.1:0",
		"-dial", m[1],
		"-replicate",
		"-peer-auth-token", "shared-secret",
		"-peer-id", "node-b",
		"-put-key", "identity-key",
		"-put-data", "identity-value",
		"-exit-after-put",
	}, &clientOut, &clientErr)
	require.NoError(t, err, "client logs=%q", clientErr.String())
	assert.Contains(t, serverErr.String(), "auth_identity=node-b")
	assert.Contains(t, clientErr.String(), "auth_identity=node-a")

	serverCancel()
	require.NoError(t, <-serverErrCh)
}

func TestRun_peerAllowIDsRejectsUnknownIdentity(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-peer-auth-token", "shared-secret",
			"-peer-id", "node-a",
			"-peer-allow-ids", "node-a",
		}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)
	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(serverOut.String())
	require.Len(t, m, 2, "stdout=%q", serverOut.String())

	var clientOut, clientErr safeBuffer
	err := run(context.Background(), []string{
		"-listen", "127.0.0.1:0",
		"-dial", m[1],
		"-replicate",
		"-peer-auth-token", "shared-secret",
		"-peer-id", "node-b",
		"-put-key", "unauthorized-key",
		"-put-data", "unauthorized-value",
		"-exit-after-put",
	}, &clientOut, &clientErr)
	require.Error(t, err, "client logs=%q", clientErr.String())
	assert.Contains(t, err.Error(), "peer auth rejected")

	serverCancel()
	require.NoError(t, <-serverErrCh)
}

func TestRun_healthExposesPeerIdentityAndAuthMetrics(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-health", "127.0.0.1:0",
			"-peer-auth-token", "shared-secret",
			"-peer-id", "node-a",
			"-peer-allow-ids", "node-b",
		}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on") && strings.Contains(serverErr.String(), "msg=health")
	}, 3*time.Second, 20*time.Millisecond)
	listenRe := regexp.MustCompile(`listening on ([^\n]+)`)
	listenMatch := listenRe.FindStringSubmatch(serverOut.String())
	require.Len(t, listenMatch, 2, "stdout=%q", serverOut.String())
	healthRe := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`)
	healthMatch := healthRe.FindStringSubmatch(serverErr.String())
	require.Len(t, healthMatch, 2, "stderr=%q", serverErr.String())

	clientCtx, clientCancel := context.WithCancel(context.Background())
	var clientOut, clientErr safeBuffer
	clientErrCh := make(chan error, 1)
	go func() {
		clientErrCh <- run(clientCtx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", listenMatch[1],
			"-replicate",
			"-peer-auth-token", "shared-secret",
			"-peer-id", "node-b",
		}, &clientOut, &clientErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverErr.String(), "auth_identity=node-b")
	}, 3*time.Second, 20*time.Millisecond, "server logs=%q client logs=%q", serverErr.String(), clientErr.String())

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + healthMatch[1]
	resp, err := client.Get(base + "/peers")
	require.NoError(t, err)
	var peers peersResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&peers))
	_ = resp.Body.Close()
	require.Len(t, peers.Peers, 1)
	assert.Equal(t, "node-b", peers.Peers[0].AuthIdentity)

	resp, err = client.Get(base + "/metrics/prometheus")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Contains(t, string(body), "streamhive_peer_auth_identity_rejections 0")

	clientCancel()
	serverCancel()
	require.NoError(t, <-clientErrCh)
	require.NoError(t, <-serverErrCh)
}

func TestRun_retriesBlobPutAfterLostAck(t *testing.T) {
	ctx := context.Background()
	receiver := p2p.NewTCPTransport("127.0.0.1:0")
	receiver.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	store := storage.NewMemoryStore()
	var puts, stored, duplicates, acks atomic.Int32
	receiver.FrameHandler = func(ctx context.Context, peer p2p.Peer, payload []byte) error {
		msg, err := replication.Decode(payload, replication.Limits{})
		if err != nil {
			return err
		}
		if msg.Type != replication.MessageTypeBlobPut {
			return nil
		}
		puts.Add(1)
		existing, err := store.Get(ctx, msg.Key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			if err := store.Put(ctx, msg.Key, msg.Data); err != nil {
				return err
			}
			stored.Add(1)
		case err != nil:
			return err
		case bytes.Equal(existing, msg.Data):
			duplicates.Add(1)
		default:
			return errors.New("retry acceptance: blob changed across retry")
		}
		if puts.Load() == 1 {
			return nil
		}
		ackPayload, err := replication.EncodeBlobAck(msg.Key, replication.Limits{})
		if err != nil {
			return err
		}
		acks.Add(1)
		return writePeerFrame(peer, ackPayload, 0)
	}
	require.NoError(t, receiver.ListenAndAccept(ctx))
	defer func() { _ = receiver.Close() }()

	var out, stderr safeBuffer
	err := run(context.Background(), []string{
		"-listen", "127.0.0.1:0",
		"-dial", receiver.Addr().String(),
		"-put-key", "retry-key",
		"-put-data", "retry-value",
		"-put-ack-timeout", "20ms",
		"-put-retries", "1",
		"-put-retry-delay", "0",
		"-exit-after-put",
	}, &out, &stderr)

	require.NoError(t, err, "sender logs=%q", stderr.String())
	assert.Equal(t, int32(2), puts.Load())
	assert.Equal(t, int32(1), stored.Load())
	assert.Equal(t, int32(1), duplicates.Load())
	assert.Equal(t, int32(1), acks.Load())
	assert.Contains(t, stderr.String(), "outcome=accepted")
	assert.Contains(t, stderr.String(), "attempts=2")
	got, err := store.Get(ctx, []byte("retry-key"))
	require.NoError(t, err)
	assert.Equal(t, []byte("retry-value"), got)
}

func TestRun_replicatesBlobPutToFileStore(t *testing.T) {
	storeDir := t.TempDir()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", storeDir,
		}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(serverOut.String())
	require.Len(t, m, 2, "stdout=%q", serverOut.String())

	var clientOut, clientErr safeBuffer
	err := run(context.Background(), []string{
		"-listen", "127.0.0.1:0",
		"-dial", m[1],
		"-put-key", "durable",
		"-put-data", "persist me",
		"-exit-after-put",
	}, &clientOut, &clientErr)
	require.NoError(t, err, "client logs=%q", clientErr.String())

	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(storeDir)
		if err != nil {
			return false
		}
		got, err := store.Get(context.Background(), []byte("durable"))
		return err == nil && string(got) == "persist me"
	}, 3*time.Second, 20*time.Millisecond, "server logs=%q", serverErr.String())

	serverCancel()
	require.NoError(t, <-serverErrCh)
}

func TestRun_replicatesContentKeyedBlobPutToFileStore(t *testing.T) {
	storeDir := t.TempDir()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	var serverOut, serverErr safeBuffer
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- run(serverCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", storeDir,
		}, &serverOut, &serverErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(serverOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(serverOut.String())
	require.Len(t, m, 2, "stdout=%q", serverOut.String())

	data := []byte("address me by content")
	var clientOut, clientErr safeBuffer
	err := run(context.Background(), []string{
		"-listen", "127.0.0.1:0",
		"-dial", m[1],
		"-put-content-key",
		"-put-data", string(data),
		"-exit-after-put",
	}, &clientOut, &clientErr)
	require.NoError(t, err, "client logs=%q", clientErr.String())

	key := storage.SHA256Key(data)
	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(storeDir)
		if err != nil {
			return false
		}
		got, err := store.Get(context.Background(), key)
		return err == nil && string(got) == string(data)
	}, 3*time.Second, 20*time.Millisecond, "server logs=%q", serverErr.String())
	assert.Contains(t, serverErr.String(), storage.SHA256KeyHex(data))

	serverCancel()
	require.NoError(t, <-serverErrCh)
}

func TestRun_syncsMissingBlobOnConnect(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	ctx := context.Background()
	data := []byte("sync me")
	key := storage.SHA256Key(data)

	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	require.NoError(t, sourceStore.Put(ctx, key, data))

	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	defer sourceCancel()
	var sourceOut, sourceErr safeBuffer
	sourceErrCh := make(chan error, 1)
	go func() {
		sourceErrCh <- run(sourceCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", sourceDir,
		}, &sourceOut, &sourceErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(sourceOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(sourceOut.String())
	require.Len(t, m, 2, "stdout=%q", sourceOut.String())

	targetCtx, targetCancel := context.WithCancel(context.Background())
	defer targetCancel()
	var targetOut, targetErr safeBuffer
	targetErrCh := make(chan error, 1)
	go func() {
		targetErrCh <- run(targetCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", targetDir,
			"-dial", m[1],
		}, &targetOut, &targetErr)
	}()

	require.Eventually(t, func() bool {
		targetStore, err := storage.NewFileStore(targetDir)
		if err != nil {
			return false
		}
		got, err := targetStore.Get(context.Background(), key)
		return err == nil && string(got) == string(data)
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())

	targetCancel()
	sourceCancel()
	require.NoError(t, <-targetErrCh)
	require.NoError(t, <-sourceErrCh)
}

func TestRun_periodicSyncsBlobAddedAfterConnect(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	ctx := context.Background()

	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)

	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	defer sourceCancel()
	var sourceOut, sourceErr safeBuffer
	sourceErrCh := make(chan error, 1)
	go func() {
		sourceErrCh <- run(sourceCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", sourceDir,
			"-sync-interval", "50ms",
		}, &sourceOut, &sourceErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(sourceOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	re := regexp.MustCompile(`listening on ([^\n]+)`)
	m := re.FindStringSubmatch(sourceOut.String())
	require.Len(t, m, 2, "stdout=%q", sourceOut.String())

	targetCtx, targetCancel := context.WithCancel(context.Background())
	defer targetCancel()
	var targetOut, targetErr safeBuffer
	targetErrCh := make(chan error, 1)
	go func() {
		targetErrCh <- run(targetCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", targetDir,
			"-dial", m[1],
		}, &targetOut, &targetErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(targetOut.String(), "listening on") &&
			strings.Contains(sourceErr.String(), "peer connected")
	}, 3*time.Second, 20*time.Millisecond)

	data := []byte("arrived after connect")
	key := storage.SHA256Key(data)
	require.NoError(t, sourceStore.Put(ctx, key, data))

	require.Eventually(t, func() bool {
		targetStore, err := storage.NewFileStore(targetDir)
		if err != nil {
			return false
		}
		got, err := targetStore.Get(context.Background(), key)
		return err == nil && string(got) == string(data)
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())

	targetCancel()
	sourceCancel()
	require.NoError(t, <-targetErrCh)
	require.NoError(t, <-sourceErrCh)
}

func TestRun_authenticatedRestartRepairsAndDeduplicatesContentBlob(t *testing.T) {
	ctx := context.Background()
	sharedSecret := "shared-secret"
	data := []byte("authenticated repair evidence")
	key := storage.SHA256Key(data)
	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	require.NoError(t, sourceStore.Put(ctx, key, data))

	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	var sourceOut, sourceErr safeBuffer
	sourceErrCh := make(chan error, 1)
	go func() {
		sourceErrCh <- run(sourceCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", sourceDir,
			"-sync-interval", "50ms",
			"-peer-auth-token", sharedSecret,
			"-peer-id", "source",
			"-peer-allow-ids", "target",
		}, &sourceOut, &sourceErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(sourceOut.String(), "listening on")
	}, 3*time.Second, 20*time.Millisecond)

	listenRe := regexp.MustCompile(`listening on ([^\n]+)`)
	sourceListen := listenRe.FindStringSubmatch(sourceOut.String())
	require.Len(t, sourceListen, 2, "source stdout=%q", sourceOut.String())

	targetArgs := []string{
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", targetDir,
		"-dial", sourceListen[1],
		"-sync-interval", "50ms",
		"-peer-auth-token", sharedSecret,
		"-peer-id", "target",
		"-peer-allow-ids", "source,sender",
	}

	startTarget := func() (context.CancelFunc, *safeBuffer, <-chan error, string, string) {
		targetCtx, targetCancel := context.WithCancel(context.Background())
		var targetOut, targetErr safeBuffer
		targetErrCh := make(chan error, 1)
		go func() {
			targetErrCh <- run(targetCtx, targetArgs, &targetOut, &targetErr)
		}()

		require.Eventually(t, func() bool {
			return strings.Contains(targetOut.String(), "listening on") &&
				strings.Contains(targetErr.String(), "msg=health")
		}, 3*time.Second, 20*time.Millisecond)

		targetListen := listenRe.FindStringSubmatch(targetOut.String())
		require.Len(t, targetListen, 2, "target stdout=%q", targetOut.String())
		healthRe := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`)
		health := healthRe.FindStringSubmatch(targetErr.String())
		require.Len(t, health, 2, "target stderr=%q", targetErr.String())
		return targetCancel, &targetErr, targetErrCh, targetListen[1], health[1]
	}

	targetCancel, targetErr, targetErrCh, _, _ := startTarget()
	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(targetDir)
		if err != nil {
			return false
		}
		got, err := store.Get(ctx, key)
		return err == nil && bytes.Equal(got, data)
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())
	require.Eventually(t, func() bool {
		return strings.Contains(sourceErr.String(), "auth_identity=target") &&
			strings.Contains(targetErr.String(), "auth_identity=source")
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())

	targetCancel()
	require.NoError(t, <-targetErrCh)

	targetStore, err := storage.NewFileStore(targetDir)
	require.NoError(t, err)
	require.NoError(t, targetStore.Delete(ctx, key))
	hasKey, err := targetStore.Has(ctx, key)
	require.NoError(t, err)
	require.False(t, hasKey)

	targetCancel, targetErr, targetErrCh, targetListen, healthAddr := startTarget()
	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(targetDir)
		if err != nil {
			return false
		}
		got, err := store.Get(ctx, key)
		return err == nil && bytes.Equal(got, data)
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())

	require.NoError(t, os.WriteFile(filepath.Join(targetDir, storage.SHA256KeyHex(data)), []byte("tampered"), 0o600))
	hasKey, err = targetStore.Has(ctx, key)
	require.NoError(t, err)
	require.False(t, hasKey)
	_, err = targetStore.Get(ctx, key)
	require.ErrorIs(t, err, storage.ErrSHA256Mismatch)
	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(targetDir)
		if err != nil {
			return false
		}
		got, err := store.Get(ctx, key)
		if err != nil || !bytes.Equal(got, data) {
			return false
		}
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + healthAddr + "/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var metrics map[string]int64
		if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
			return false
		}
		return metrics["replication_corrupt_blobs_detected"] >= 1
	}, 3*time.Second, 20*time.Millisecond, "source logs=%q target logs=%q", sourceErr.String(), targetErr.String())

	var senderErr safeBuffer
	err = run(ctx, []string{
		"-listen", "127.0.0.1:0",
		"-dial", targetListen,
		"-peer-auth-token", sharedSecret,
		"-peer-id", "sender",
		"-put-content-key",
		"-put-data", string(data),
		"-exit-after-put",
	}, io.Discard, &senderErr)
	require.NoError(t, err, "sender logs=%q", senderErr.String())
	require.Eventually(t, func() bool {
		return strings.Contains(targetErr.String(), "auth_identity=sender")
	}, 3*time.Second, 20*time.Millisecond, "target logs=%q sender logs=%q", targetErr.String(), senderErr.String())

	client := &http.Client{Timeout: 2 * time.Second}
	require.Eventually(t, func() bool {
		resp, err := client.Get("http://" + healthAddr + "/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var metrics map[string]int64
		if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
			return false
		}
		return metrics["replication_duplicate_blobs"] >= 1
	}, 3*time.Second, 20*time.Millisecond, "target logs=%q sender logs=%q", targetErr.String(), senderErr.String())

	got, err := targetStore.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	targetCancel()
	sourceCancel()
	require.NoError(t, <-targetErrCh)
	require.NoError(t, <-sourceErrCh)
}

func TestParsePeerTargets(t *testing.T) {
	tests := []struct {
		name    string
		dial    string
		peers   string
		want    []string
		wantErr bool
	}{
		{
			name: "single dial",
			dial: " 127.0.0.1:7070 ",
			want: []string{"127.0.0.1:7070"},
		},
		{
			name:  "peer list",
			peers: "127.0.0.1:7070, 127.0.0.1:7071",
			want:  []string{"127.0.0.1:7070", "127.0.0.1:7071"},
		},
		{
			name:  "dial plus peer list",
			dial:  "127.0.0.1:7070",
			peers: "127.0.0.1:7071",
			want:  []string{"127.0.0.1:7070", "127.0.0.1:7071"},
		},
		{
			name:    "empty peer entry",
			peers:   "127.0.0.1:7070,",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePeerTargets(tt.dial, tt.peers)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePeerIdentityList(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "empty", want: nil},
		{name: "trimmed list", input: "node-a, node-b", want: []string{"node-a", "node-b"}},
		{name: "empty entry", input: "node-a,", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePeerIdentityList(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMissingKeys(t *testing.T) {
	missing := missingKeys(
		[][]byte{[]byte("a"), []byte("b"), []byte("c")},
		[][]byte{[]byte("b")},
	)
	require.Equal(t, [][]byte{[]byte("a"), []byte("c")}, missing)

	missing[0][0] = 'x'
	again := missingKeys(
		[][]byte{[]byte("a")},
		nil,
	)
	require.Equal(t, [][]byte{[]byte("a")}, again)
}

func TestHandleReplicationMessageSkipsExactDuplicateBlobPut(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	data := []byte("same")
	key := storage.SHA256Key(data)
	msg := replication.Message{
		Type: replication.MessageTypeBlobPut,
		Key:  key,
		Data: data,
	}

	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))
	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))

	assert.Equal(t, uint64(1), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(1), metrics.DuplicateBlobs.Load())
	assert.Equal(t, uint64(len(data)), metrics.DuplicateBytes.Load())
	assert.Equal(t, uint64(2), metrics.BlobAcksSent.Load())
	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestHandleReplicationMessageSendsAckAfterBlobPut(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	msg := replication.Message{
		Type: replication.MessageTypeBlobPut,
		Key:  []byte("manual"),
		Data: []byte("value"),
	}

	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))

	require.Len(t, peer.payloads, 1)
	ack, err := replication.Decode(peer.payloads[0], replication.Limits{})
	require.NoError(t, err)
	assert.Equal(t, replication.MessageTypeBlobAck, ack.Type)
	assert.Equal(t, []byte("manual"), ack.Key)
	assert.Equal(t, uint64(1), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(1), metrics.BlobAcksSent.Load())
}

func TestHandleReplicationMessageCountsBlobAck(t *testing.T) {
	metrics := &replicationMetrics{}
	tracker := newPutAckTracker(metrics)
	metrics.ackTracker = tracker
	peer := testPeer{}
	msg := replication.Message{
		Type: replication.MessageTypeBlobAck,
		Key:  []byte("manual"),
	}
	ackCh := tracker.register(peer, msg.Key)

	require.NoError(t, handleReplicationMessage(context.Background(), peer, storage.NewMemoryStore(), nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))

	assert.Equal(t, uint64(1), metrics.BlobAcksReceived.Load())
	assert.Equal(t, uint64(1), metrics.BlobAcksMatched.Load())
	assert.Equal(t, int64(0), metrics.BlobAcksPending.Load())
	select {
	case <-ackCh:
	default:
		t.Fatal("blob ACK did not close the pending waiter")
	}
}

func TestSendBlobWithAckRetriesUntilAcknowledged(t *testing.T) {
	metrics := &replicationMetrics{}
	tracker := newPutAckTracker(metrics)
	peer := &retryPeer{writeEvents: make(chan struct{}, 4)}
	key := []byte("manual")
	errCh := make(chan error, 1)
	go func() {
		errCh <- sendBlobWithAck(
			context.Background(),
			peer,
			[]byte("blob frame"),
			key,
			0,
			0,
			tracker,
			5*time.Millisecond,
			1,
			time.Millisecond,
			metrics,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	}()

	<-peer.writeEvents
	<-peer.writeEvents
	require.True(t, tracker.ack(peer, key))
	require.NoError(t, <-errCh)
	assert.Equal(t, int32(2), peer.writes.Load())
	assert.Equal(t, uint64(1), metrics.BlobRetries.Load())
	assert.Equal(t, uint64(1), metrics.BlobAckTimeouts.Load())
	assert.Equal(t, uint64(1), metrics.BlobAcksMatched.Load())
	assert.Equal(t, uint64(1), metrics.BlobPutsAccepted.Load())
	assert.Equal(t, uint64(0), metrics.BlobPutFailures.Load())
	assert.Equal(t, int64(0), metrics.BlobAcksPending.Load())
}

func TestSendBlobWithAckStopsAfterBoundedRetries(t *testing.T) {
	metrics := &replicationMetrics{}
	tracker := newPutAckTracker(metrics)
	peer := &retryPeer{writeEvents: make(chan struct{}, 4)}

	err := sendBlobWithAck(
		context.Background(),
		peer,
		[]byte("blob frame"),
		[]byte("manual"),
		0,
		0,
		tracker,
		1*time.Millisecond,
		2,
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	assert.ErrorIs(t, err, errBlobAckTimeout)
	assert.Equal(t, int32(3), peer.writes.Load())
	assert.Equal(t, uint64(2), metrics.BlobRetries.Load())
	assert.Equal(t, uint64(3), metrics.BlobAckTimeouts.Load())
	assert.Equal(t, uint64(0), metrics.BlobPutsAccepted.Load())
	assert.Equal(t, uint64(1), metrics.BlobPutFailures.Load())
	assert.Equal(t, int64(0), metrics.BlobAcksPending.Load())
}

func TestSendBlobWithAckClosesPeerOnWriteFailure(t *testing.T) {
	metrics := &replicationMetrics{}
	tracker := newPutAckTracker(metrics)
	writeErr := errors.New("broken stream")
	peer := &writeFailurePeer{err: writeErr}

	err := sendBlobWithAck(
		context.Background(),
		peer,
		[]byte("blob frame"),
		[]byte("manual"),
		0,
		0,
		tracker,
		5*time.Millisecond,
		0,
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	assert.ErrorIs(t, err, writeErr)
	var deliveryErr *blobDeliveryError
	require.ErrorAs(t, err, &deliveryErr)
	assert.Equal(t, "write-error", deliveryErr.kind)
	assert.Equal(t, 1, deliveryErr.attempts)
	assert.Equal(t, int32(1), peer.closes.Load())
	assert.Equal(t, uint64(1), metrics.BlobWriteErrors.Load())
	assert.Equal(t, uint64(1), metrics.BlobPutFailures.Load())
	assert.Equal(t, uint64(0), metrics.BlobPutsAccepted.Load())
	assert.Equal(t, int64(0), metrics.BlobAcksPending.Load())
}

func TestHandleReplicationMessageRejectsSHA256Mismatch(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}
	msg := replication.Message{
		Type: replication.MessageTypeBlobPut,
		Key:  storage.SHA256Key([]byte("expected")),
		Data: []byte("tampered"),
	}

	err := handleReplicationMessage(ctx, testPeer{}, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	assert.ErrorIs(t, err, storage.ErrSHA256Mismatch)
	assert.Equal(t, uint64(0), metrics.BlobsStored.Load())
}

func TestHandleReplicationMessageReplacesCorruptFileStoreContentBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.NewFileStore(dir)
	require.NoError(t, err)
	data := []byte("repairable content")
	key := storage.SHA256Key(data)
	require.NoError(t, store.Put(ctx, key, data))
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.SHA256KeyHex(data)), []byte("tampered"), 0o600))

	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	err = handleReplicationMessage(ctx, peer, store, nil, replication.Message{
		Type: replication.MessageTypeBlobPut,
		Key:  key,
		Data: data,
	}, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, err)

	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, uint64(1), metrics.CorruptBlobsDetected.Load())
	assert.Equal(t, uint64(1), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(1), metrics.BlobAcksSent.Load())
}

func TestHandleReplicationMessageAllowsOpaqueKeyReplace(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	first := replication.Message{Type: replication.MessageTypeBlobPut, Key: []byte("manual"), Data: []byte("first")}
	second := replication.Message{Type: replication.MessageTypeBlobPut, Key: []byte("manual"), Data: []byte("second")}
	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, first, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))
	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, second, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))

	assert.Equal(t, uint64(2), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(0), metrics.DuplicateBlobs.Load())
	assert.Equal(t, uint64(2), metrics.BlobAcksSent.Load())
	got, err := store.Get(ctx, []byte("manual"))
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), got)
}

func TestResolvePutKey(t *testing.T) {
	key, label := resolvePutKey("manual", []byte("hello"), false)
	assert.Equal(t, []byte("manual"), key)
	assert.Equal(t, "manual", label)

	key, label = resolvePutKey("", []byte("hello"), true)
	assert.Equal(t, storage.SHA256Key([]byte("hello")), key)
	assert.Equal(t, storage.SHA256KeyHex([]byte("hello")), label)
}

func TestFormatBlobKey(t *testing.T) {
	data := []byte("hello")
	assert.Equal(t, "manual", formatBlobKey([]byte("manual")))
	assert.Equal(t, storage.SHA256KeyHex(data), formatBlobKey(storage.SHA256Key(data)))
}

func TestVerifyContentKeyIfSHA256(t *testing.T) {
	data := []byte("hello")
	assert.NoError(t, verifyContentKeyIfSHA256([]byte("manual"), data))
	assert.NoError(t, verifyContentKeyIfSHA256(storage.SHA256Key(data), data))
	assert.ErrorIs(t, verifyContentKeyIfSHA256(storage.SHA256Key(data), []byte("tampered")), storage.ErrSHA256Mismatch)
}

func TestSendBlobHasCountsInventoryAdvertisement(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("inventory-key"), []byte("value")))
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	require.NoError(t, sendBlobHas(ctx, peer, store, replication.Limits{}, 0, metrics))
	require.Len(t, peer.payloads, 1)
	msg, err := replication.Decode(peer.payloads[0], replication.Limits{})
	require.NoError(t, err)
	assert.Equal(t, replication.MessageTypeBlobHas, msg.Type)
	assert.Equal(t, [][]byte{[]byte("inventory-key")}, msg.Keys)
	assert.Equal(t, uint64(1), metrics.InventoryAdvertisements.Load())
}

func TestSendBlobHasBatchesAtConfiguredKeyLimit(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	for _, key := range keys {
		require.NoError(t, store.Put(ctx, key, []byte("value")))
	}
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	require.NoError(t, sendBlobHas(ctx, peer, store, replication.Limits{MaxKeys: 2}, 0, metrics))
	require.Len(t, peer.payloads, 3)
	var advertised [][]byte
	for _, payload := range peer.payloads {
		msg, err := replication.Decode(payload, replication.Limits{MaxKeys: 2})
		require.NoError(t, err)
		assert.Equal(t, replication.MessageTypeBlobHas, msg.Type)
		advertised = append(advertised, msg.Keys...)
	}
	assert.Equal(t, keys, advertised)
	assert.Equal(t, uint64(3), metrics.InventoryAdvertisements.Load())
}

func TestSendBlobHasSplitsAtFrameLimit(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	for _, key := range keys {
		require.NoError(t, store.Put(ctx, key, []byte("value")))
	}
	single, err := replication.EncodeBlobHas([][]byte{keys[0]}, replication.Limits{})
	require.NoError(t, err)
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	require.NoError(t, sendBlobHas(ctx, peer, store, replication.Limits{}, len(single), metrics))
	require.Len(t, peer.payloads, len(keys))
	for _, payload := range peer.payloads {
		assert.LessOrEqual(t, len(payload), len(single))
	}
	assert.Equal(t, uint64(len(keys)), metrics.InventoryAdvertisements.Load())
}

func TestSendBlobHasRejectsSingleKeyOverFrameLimit(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("value")))
	single, err := replication.EncodeBlobHas([][]byte{[]byte("a")}, replication.Limits{})
	require.NoError(t, err)
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	err = sendBlobHas(ctx, peer, store, replication.Limits{}, len(single)-1, metrics)
	assert.ErrorIs(t, err, p2p.ErrFrameTooLarge)
	assert.Empty(t, peer.payloads)
	assert.Equal(t, uint64(0), metrics.InventoryAdvertisements.Load())
}

func TestHandleReplicationMessageCountsMissingKeysRequested(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("already-local"), []byte("value")))
	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	msg := replication.Message{
		Type: replication.MessageTypeBlobHas,
		Keys: [][]byte{[]byte("already-local"), []byte("needs-repair")},
	}

	require.NoError(t, handleReplicationMessage(ctx, peer, store, store, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))
	require.Len(t, peer.payloads, 1)
	missing, err := replication.Decode(peer.payloads[0], replication.Limits{})
	require.NoError(t, err)
	assert.Equal(t, replication.MessageTypeBlobMissing, missing.Type)
	assert.Equal(t, [][]byte{[]byte("needs-repair")}, missing.Keys)
	assert.Equal(t, uint64(1), metrics.MissingKeysRequested.Load())
}

func TestHandleReplicationMessageChecksAdvertisedKeysWithoutLister(t *testing.T) {
	ctx := context.Background()
	base := storage.NewMemoryStore()
	require.NoError(t, base.Put(ctx, []byte("already-local"), []byte("value")))
	store := &hasProbeStore{BlobStore: base}
	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	msg := replication.Message{
		Type: replication.MessageTypeBlobHas,
		Keys: [][]byte{[]byte("already-local"), []byte("needs-repair")},
	}

	require.NoError(t, handleReplicationMessage(ctx, peer, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))
	require.Len(t, peer.payloads, 1)
	missing, err := replication.Decode(peer.payloads[0], replication.Limits{})
	require.NoError(t, err)
	assert.Equal(t, replication.MessageTypeBlobMissing, missing.Type)
	assert.Equal(t, [][]byte{[]byte("needs-repair")}, missing.Keys)
	assert.Equal(t, int32(2), store.hasCalls.Load())
	assert.Equal(t, uint64(1), metrics.MissingKeysRequested.Load())
}

func TestSendRequestedBlobsSkipsUnsendableBlobAndContinues(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("too-large"), []byte("oversized")))
	require.NoError(t, store.Put(ctx, []byte("ok"), []byte("yo")))
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	err := sendRequestedBlobs(
		ctx,
		peer,
		store,
		[][]byte{[]byte("too-large"), []byte("ok")},
		replication.Limits{MaxDataBytes: 4},
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		true,
	)

	require.NoError(t, err)
	require.Len(t, peer.payloads, 1)
	msg, err := replication.Decode(peer.payloads[0], replication.Limits{MaxDataBytes: 4})
	require.NoError(t, err)
	assert.Equal(t, replication.MessageTypeBlobPut, msg.Type)
	assert.Equal(t, []byte("ok"), msg.Key)
	assert.Equal(t, []byte("yo"), msg.Data)
	assert.Equal(t, uint64(1), metrics.BlobsSent.Load())
	assert.Equal(t, uint64(1), metrics.BlobsSkipped.Load())
	assert.Equal(t, uint64(1), metrics.SendErrors.Load())
	assert.Equal(t, uint64(1), metrics.RepairBlobsSent.Load())
}

func TestSendRequestedBlobsDefersAfterRepairByteBudgetAndRecovers(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("data")))
	}
	limits := replication.Limits{MaxDataBytes: 16, MaxRepairBytes: 8}
	metrics := &replicationMetrics{}
	peer := &capturePeer{}
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}

	require.NoError(t, sendRequestedBlobs(
		ctx,
		peer,
		store,
		keys,
		limits,
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		true,
	))
	assert.Len(t, peer.payloads, 2)
	assert.Equal(t, uint64(2), metrics.RepairBlobsSent.Load())
	assert.Equal(t, uint64(1), metrics.RepairBlobsDeferred.Load())

	require.NoError(t, sendRequestedBlobs(
		ctx,
		peer,
		store,
		[][]byte{keys[2]},
		limits,
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		true,
	))
	assert.Len(t, peer.payloads, 3)
	assert.Equal(t, uint64(3), metrics.RepairBlobsSent.Load())
	assert.Equal(t, uint64(1), metrics.RepairBlobsDeferred.Load())
}

func TestRepairContinuationSchedulerDeduplicatesAndCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemoryStore()
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("data")))
	}
	limits := replication.Limits{MaxDataBytes: 16, MaxRepairBytes: 8}
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(ctx, store, limits, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), 100*time.Millisecond)
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}

	scheduler.Schedule(peer, keys)
	scheduler.Schedule(peer, keys)

	require.Eventually(t, func() bool { return peer.Len() == 3 }, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return len(scheduler.entries) == 0
	}, time.Second, 5*time.Millisecond)

	payloads := peer.Payloads()
	gotKeys := make([][]byte, 0, len(payloads))
	for _, payload := range payloads {
		msg, err := replication.Decode(payload, limits)
		require.NoError(t, err)
		gotKeys = append(gotKeys, msg.Key)
	}
	assert.Equal(t, keys, gotKeys)
	assert.Equal(t, uint64(3), metrics.RepairBlobsSent.Load())
	assert.Equal(t, uint64(1), metrics.RepairBlobsDeferred.Load())
	assert.Equal(t, uint64(2), metrics.RepairContinuationsScheduled.Load())
	assert.Equal(t, uint64(2), metrics.RepairContinuationsCompleted.Load())
	assert.Equal(t, uint64(0), metrics.RepairContinuationsDropped.Load())
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestRepairContinuationSchedulerKeepsPeersIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemoryStore()
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("data")))
	}
	limits := replication.Limits{MaxDataBytes: 16, MaxRepairBytes: 8}
	metrics := &replicationMetrics{}
	releaseSlow := make(chan struct{})
	var releaseSlowOnce sync.Once
	defer func() { releaseSlowOnce.Do(func() { close(releaseSlow) }) }()
	slow := &asyncCapturePeer{
		addr:         &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7071},
		writeStarted: make(chan struct{}),
		writeRelease: releaseSlow,
	}
	healthy := &asyncCapturePeer{
		addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7072},
	}
	scheduler := newRepairContinuationScheduler(ctx, store, limits, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}

	scheduler.Schedule(slow, keys)
	select {
	case <-slow.writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow peer did not reach its gated write")
	}

	scheduler.Schedule(healthy, keys)
	require.Eventually(t, func() bool {
		return healthy.Len() == 3 && metrics.RepairContinuationsCompleted.Load() >= 2
	}, 2*time.Second, 5*time.Millisecond)

	scheduler.mu.Lock()
	slowEntry := scheduler.entries[repairPeerKey(slow)]
	assert.NotNil(t, slowEntry)
	if slowEntry != nil {
		assert.True(t, slowEntry.running)
		assert.LessOrEqual(t, len(slowEntry.pending), scheduler.maxKeys)
	}
	scheduler.mu.Unlock()
	assert.Zero(t, slow.Len())
	assert.Equal(t, int64(1), metrics.RepairContinuationsActive.Load())
	assert.Equal(t, int64(3), metrics.RepairContinuationKeysPending.Load())

	releaseSlowOnce.Do(func() { close(releaseSlow) })
	require.Eventually(t, func() bool {
		return slow.Len() == 3 && metrics.RepairContinuationsCompleted.Load() == 4
	}, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return len(scheduler.entries) == 0
	}, time.Second, 5*time.Millisecond)
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestHandleReplicationMessageSchedulesRepairContinuation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemoryStore()
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(ctx, []byte(key), []byte("data")))
	}
	limits := replication.Limits{MaxDataBytes: 16, MaxRepairBytes: 8}
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(ctx, store, limits, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), 100*time.Millisecond)
	metrics.repairScheduler = scheduler

	require.NoError(t, handleReplicationMessage(
		ctx,
		peer,
		store,
		nil,
		replication.Message{
			Type: replication.MessageTypeBlobMissing,
			Keys: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
		},
		limits,
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		store,
	))

	require.Eventually(t, func() bool {
		return peer.Len() == 3 && metrics.RepairContinuationsCompleted.Load() == 1
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, uint64(3), metrics.RepairBlobsSent.Load())
	assert.Equal(t, uint64(1), metrics.RepairBlobsDeferred.Load())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsScheduled.Load())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsCompleted.Load())
	assert.Equal(t, uint64(0), metrics.RepairContinuationsDropped.Load())
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestRepairContinuationSchedulerForgetsDisconnectedPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("data")))
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(ctx, store, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), 100*time.Millisecond)

	scheduler.Schedule(peer, [][]byte{[]byte("a")})
	scheduler.Forget(peer)
	time.Sleep(2 * scheduler.delay)

	assert.Zero(t, peer.Len())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsScheduled.Load())
	assert.Equal(t, uint64(0), metrics.RepairContinuationsCompleted.Load())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsDropped.Load())
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
	scheduler.mu.Lock()
	assert.Empty(t, scheduler.entries)
	scheduler.mu.Unlock()
}

func TestRepairContinuationSchedulerBoundsQueueAndCountsDrops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(ctx, store, replication.Limits{MaxKeys: 1}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)

	scheduler.Schedule(peer, [][]byte{[]byte("a")})
	scheduler.Schedule(peer, [][]byte{[]byte("b")})
	assert.Equal(t, int64(1), metrics.RepairContinuationsActive.Load())
	assert.Equal(t, int64(1), metrics.RepairContinuationKeysPending.Load())
	scheduler.Forget(peer)

	assert.Equal(t, uint64(1), metrics.RepairContinuationsScheduled.Load())
	assert.Equal(t, uint64(0), metrics.RepairContinuationsCompleted.Load())
	assert.Equal(t, uint64(2), metrics.RepairContinuationsDropped.Load())
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestRepairContinuationSchedulerStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(context.Background(), []byte("a"), []byte("data")))
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(ctx, store, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), 100*time.Millisecond)

	scheduler.Schedule(peer, [][]byte{[]byte("a")})
	cancel()
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return len(scheduler.entries) == 0
	}, time.Second, 5*time.Millisecond)
	time.Sleep(2 * scheduler.delay)

	assert.Zero(t, peer.Len())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsScheduled.Load())
	assert.Equal(t, uint64(0), metrics.RepairContinuationsCompleted.Load())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsDropped.Load())
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestReplicationMetricsSnapshotContinuationCountersAreMonotonic(t *testing.T) {
	metrics := &replicationMetrics{}
	initial := metrics.Snapshot()
	metrics.RepairContinuationsScheduled.Add(2)
	metrics.RepairContinuationsCompleted.Add(1)
	metrics.RepairContinuationsDropped.Add(1)
	first := metrics.Snapshot()
	second := metrics.Snapshot()

	assert.Equal(t, int64(0), initial["replication_repair_continuations_scheduled"])
	assert.Equal(t, int64(0), initial["replication_repair_continuations_active"])
	assert.Equal(t, int64(0), initial["replication_repair_continuation_keys_pending"])
	assert.Equal(t, int64(2), first["replication_repair_continuations_scheduled"])
	assert.Equal(t, int64(1), first["replication_repair_continuations_completed"])
	assert.Equal(t, int64(1), first["replication_repair_continuations_dropped"])
	assert.Equal(t, first["replication_repair_continuations_scheduled"], second["replication_repair_continuations_scheduled"])
	assert.Equal(t, first["replication_repair_continuations_completed"], second["replication_repair_continuations_completed"])
	assert.Equal(t, first["replication_repair_continuations_dropped"], second["replication_repair_continuations_dropped"])
}

func TestSendRequestedBlobsStopsWhenContextCancelsAfterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := storage.NewMemoryStore()
	key := []byte("a")
	require.NoError(t, base.Put(ctx, key, []byte("first")))
	store := &cancelAfterGetStore{BlobStore: base, cancel: cancel}
	metrics := &replicationMetrics{}
	peer := &capturePeer{}

	err := sendRequestedBlobs(
		ctx,
		peer,
		store,
		[][]byte{key},
		replication.Limits{},
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		true,
	)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, peer.payloads)
	assert.Equal(t, uint64(0), metrics.BlobsSent.Load())
}

func TestSendRequestedBlobsStopsOnFrameWriteError(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("a"), []byte("first")))
	require.NoError(t, store.Put(ctx, []byte("b"), []byte("second")))
	metrics := &replicationMetrics{}
	writeErr := errors.New("write failed")
	peer := &capturePeer{err: writeErr}

	err := sendRequestedBlobs(
		ctx,
		peer,
		store,
		[][]byte{[]byte("a"), []byte("b")},
		replication.Limits{},
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		false,
	)

	assert.ErrorIs(t, err, writeErr)
	assert.Empty(t, peer.payloads)
	assert.Equal(t, uint64(0), metrics.BlobsSent.Load())
	assert.Equal(t, uint64(0), metrics.BlobsSkipped.Load())
	assert.Equal(t, uint64(1), metrics.SendErrors.Load())
}

func TestWritePrometheusMetrics(t *testing.T) {
	var out bytes.Buffer
	writePrometheusMetrics(&out, map[string]int64{
		"z_metric": 2,
		"a_metric": 1,
	})
	assert.Equal(t, "streamhive_a_metric 1\nstreamhive_z_metric 2\n", out.String())
}

func TestSnapshotPeersSortsByAddress(t *testing.T) {
	connectedAt := time.Date(2026, 7, 2, 1, 2, 3, 4, time.UTC)
	now := connectedAt.Add(1500 * time.Millisecond)
	resp := snapshotPeers([]p2p.PeerSnapshot{
		{RemoteAddr: "127.0.0.1:9002", LocalAddr: "127.0.0.1:7002", Outbound: true, ConnectedAt: connectedAt},
		{RemoteAddr: "127.0.0.1:9001", LocalAddr: "127.0.0.1:7001", Outbound: false, ConnectedAt: connectedAt},
	}, now)

	require.Equal(t, 2, resp.ActivePeers)
	require.Equal(t, []peerStatus{
		{
			RemoteAddr:     "127.0.0.1:9001",
			LocalAddr:      "127.0.0.1:7001",
			Outbound:       false,
			ConnectedAt:    "2026-07-02T01:02:03.000000004Z",
			ConnectedForMS: 1500,
			AuthMethod:     "",
		},
		{
			RemoteAddr:     "127.0.0.1:9002",
			LocalAddr:      "127.0.0.1:7002",
			Outbound:       true,
			ConnectedAt:    "2026-07-02T01:02:03.000000004Z",
			ConnectedForMS: 1500,
			AuthMethod:     "",
		},
	}, resp.Peers)
}

func TestSnapshotPeersIncludesAuthMethod(t *testing.T) {
	resp := snapshotPeers([]p2p.PeerSnapshot{
		{RemoteAddr: "127.0.0.1:9001", AuthMethod: p2p.PeerAuthMethodSharedToken},
	}, time.Now().UTC())

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, p2p.PeerAuthMethodSharedToken, resp.Peers[0].AuthMethod)
}

func TestSnapshotPeersIncludesAuthIdentity(t *testing.T) {
	resp := snapshotPeers([]p2p.PeerSnapshot{
		{RemoteAddr: "127.0.0.1:9001", AuthIdentity: "node-a"},
	}, time.Now().UTC())

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, "node-a", resp.Peers[0].AuthIdentity)
}

func TestValidateReconnectBackoff(t *testing.T) {
	assert.NoError(t, validateReconnectBackoff(10*time.Millisecond, 20*time.Millisecond))
	assert.Error(t, validateReconnectBackoff(0, 20*time.Millisecond))
	assert.Error(t, validateReconnectBackoff(20*time.Millisecond, 10*time.Millisecond))
}

func TestPeerReconnector_dialsWhenPeerAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := p2p.NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client.DialTimeout = 20 * time.Millisecond
	require.NoError(t, client.ListenAndAccept(ctx))
	defer func() { _ = client.Close() }()

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	reconnector := newPeerReconnector(
		ctx,
		client,
		[]string{addr},
		10*time.Millisecond,
		20*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconnector.Start()

	var seen atomic.Int32
	server := p2p.NewTCPTransport(addr)
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.OnPeer = func(p2p.Peer) {
		seen.Add(1)
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	defer func() { _ = server.Close() }()

	require.Eventually(t, func() bool {
		return seen.Load() == 1
	}, 3*time.Second, 20*time.Millisecond)
}
