package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_storageIntegrityStatusReportsAggregateHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storeDir := t.TempDir()
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)

	validData := []byte("storage integrity verified")
	validKey := storage.SHA256Key(validData)
	corruptExpectedData := []byte("storage integrity expected")
	corruptKey := storage.SHA256Key(corruptExpectedData)
	opaqueKey := []byte("storage-integrity-opaque")
	opaqueData := []byte("opaque payload")
	require.NoError(t, store.Put(ctx, validKey, validData))
	require.NoError(t, store.Put(ctx, corruptKey, []byte("tampered payload")))
	require.NoError(t, store.Put(ctx, opaqueKey, opaqueData))
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, storage.SHA256KeyHex(corruptExpectedData)), []byte("tampered payload"), 0o600))

	var out, stderr safeBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-health", "127.0.0.1:0",
			"-replicate",
			"-store-dir", storeDir,
		}, &out, &stderr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "listening on") && strings.Contains(stderr.String(), "msg=health")
	}, 3*time.Second, 20*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())
	healthMatch := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`).FindStringSubmatch(stderr.String())
	require.Len(t, healthMatch, 2, "stderr=%q", stderr.String())
	base := "http://" + healthMatch[1]
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(base + "/storage/status")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var status storageIntegrityStatusResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&status))
	require.NoError(t, resp.Body.Close())
	assert.True(t, status.Enabled)
	assert.False(t, status.Healthy)
	assert.Equal(t, "live", status.ScanConsistency)
	assert.Equal(t, 3, status.Keys)
	assert.Equal(t, int64(len(validKey)+len(corruptKey)+len(opaqueKey)), status.KeyBytes)
	assert.Equal(t, 2, status.ContentAddressedKeys)
	assert.Equal(t, 1, status.VerifiedKeys)
	assert.Equal(t, int64(len(validData)), status.VerifiedBytes)
	assert.Equal(t, 1, status.OpaqueKeys)
	assert.Equal(t, int64(len(opaqueData)), status.OpaqueBytes)
	assert.Equal(t, 1, status.CorruptKeys)
	assert.Zero(t, status.MissingKeys)

	resp, err = client.Get(base + "/metrics")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var metrics map[string]int64
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&metrics))
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, int64(1), metrics["replication_storage_integrity_scans_started"])
	assert.Equal(t, int64(1), metrics["replication_storage_integrity_scans_completed"])
	assert.Zero(t, metrics["replication_storage_integrity_scans_failed"])
	assert.Equal(t, int64(3), metrics["replication_storage_integrity_keys_scanned"])
	assert.Equal(t, int64(len(validKey)+len(corruptKey)+len(opaqueKey)), metrics["replication_storage_integrity_key_bytes_scanned"])
	assert.Equal(t, int64(2), metrics["replication_storage_integrity_content_addressed_keys"])
	assert.Equal(t, int64(1), metrics["replication_storage_integrity_verified_keys"])
	assert.Equal(t, int64(len(validData)), metrics["replication_storage_integrity_verified_bytes"])
	assert.Equal(t, int64(1), metrics["replication_storage_integrity_opaque_keys"])
	assert.Equal(t, int64(len(opaqueData)), metrics["replication_storage_integrity_opaque_bytes"])
	assert.Equal(t, int64(1), metrics["replication_storage_integrity_corrupt_keys"])
	assert.Zero(t, metrics["replication_storage_integrity_missing_keys"])

	resp, err = client.Get(base + "/metrics/prometheus")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "streamhive_replication_storage_integrity_scans_completed 1\n")
	assert.Contains(t, string(body), "streamhive_replication_storage_integrity_verified_keys 1\n")
	assert.Contains(t, string(body), "streamhive_replication_storage_integrity_corrupt_keys 1\n")
	assert.Contains(t, string(body), "streamhive_replication_storage_integrity_missing_keys 0\n")

	cancel()
	require.NoError(t, <-errCh, "stderr=%q", stderr.String())
}
