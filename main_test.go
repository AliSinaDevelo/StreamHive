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

func (p *capturePeer) WriteFrame(payload []byte, _ int) error {
	if p.err != nil {
		return p.err
	}
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
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

	resp4, err := client.Get(base + "/metrics/prometheus")
	require.NoError(t, err)
	defer func() { _ = resp4.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp4.StatusCode)
	body, err := io.ReadAll(resp4.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "streamhive_active_peers")
	assert.Contains(t, string(body), "streamhive_replication_blobs_stored")

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

func TestRun_syncIntervalRejectsNegative(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-sync-interval", "-1s"}, &out, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-sync-interval must be zero or greater")
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
	data := []byte("same")
	key := storage.SHA256Key(data)
	msg := replication.Message{
		Type: replication.MessageTypeBlobPut,
		Key:  key,
		Data: data,
	}

	require.NoError(t, handleReplicationMessage(ctx, testPeer{}, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))
	require.NoError(t, handleReplicationMessage(ctx, testPeer{}, store, nil, msg, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))

	assert.Equal(t, uint64(1), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(1), metrics.DuplicateBlobs.Load())
	assert.Equal(t, uint64(len(data)), metrics.DuplicateBytes.Load())
	got, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, data, got)
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

func TestHandleReplicationMessageAllowsOpaqueKeyReplace(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	metrics := &replicationMetrics{}

	first := replication.Message{Type: replication.MessageTypeBlobPut, Key: []byte("manual"), Data: []byte("first")}
	second := replication.Message{Type: replication.MessageTypeBlobPut, Key: []byte("manual"), Data: []byte("second")}
	require.NoError(t, handleReplicationMessage(ctx, testPeer{}, store, nil, first, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))
	require.NoError(t, handleReplicationMessage(ctx, testPeer{}, store, nil, second, replication.Limits{}, 0, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), store))

	assert.Equal(t, uint64(2), metrics.BlobsStored.Load())
	assert.Equal(t, uint64(0), metrics.DuplicateBlobs.Load())
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
		},
		{
			RemoteAddr:     "127.0.0.1:9002",
			LocalAddr:      "127.0.0.1:7002",
			Outbound:       true,
			ConnectedAt:    "2026-07-02T01:02:03.000000004Z",
			ConnectedForMS: 1500,
		},
	}, resp.Peers)
}

func TestValidateReconnectBackoff(t *testing.T) {
	assert.NoError(t, validateReconnectBackoff(10*time.Millisecond, 20*time.Millisecond))
	assert.Error(t, validateReconnectBackoff(0, 20*time.Millisecond))
	assert.Error(t, validateReconnectBackoff(20*time.Millisecond, 10*time.Millisecond))
}

func TestPeerReconnector_dialsWhenPeerAppears(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := p2p.NewTCPTransport("127.0.0.1:0")
	client.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client.DialTimeout = 20 * time.Millisecond
	require.NoError(t, client.ListenAndAccept(ctx))
	defer func() { _ = client.Close() }()

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
