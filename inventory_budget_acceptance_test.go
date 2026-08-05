package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	budgetedInventoryBytes = "128"
	budgetedInventoryKeys  = "1"
)

type inventoryBudgetNode struct {
	cancel context.CancelFunc
	errCh  chan error
	out    safeBuffer
	err    safeBuffer
	listen string
	health string
}

func startInventoryBudgetNode(t *testing.T, storeDir, dial string) *inventoryBudgetNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	node := &inventoryBudgetNode{
		cancel: cancel,
		errCh:  make(chan error, 1),
	}
	args := []string{
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", storeDir,
		"-sync-interval", "0s",
		"-max-inventory-bytes", budgetedInventoryBytes,
		"-max-inventory-keys", budgetedInventoryKeys,
	}
	if dial != "" {
		args = append(args, "-dial", dial)
	}
	go func() {
		node.errCh <- run(ctx, args, &node.out, &node.err)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(node.out.String(), "listening on") &&
			strings.Contains(node.err.String(), "msg=health")
	}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", node.out.String(), node.err.String())

	listenMatch := regexp.MustCompile(`listening on ([^\n]+)`).FindStringSubmatch(node.out.String())
	require.Len(t, listenMatch, 2, "stdout=%q", node.out.String())
	healthMatch := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`).FindStringSubmatch(node.err.String())
	require.Len(t, healthMatch, 2, "stderr=%q", node.err.String())
	node.listen = listenMatch[1]
	node.health = healthMatch[1]
	return node
}

func (n *inventoryBudgetNode) stop(t *testing.T) {
	t.Helper()
	if n == nil || n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case err := <-n.errCh:
		require.NoError(t, err, "node stderr=%q", n.err.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("node did not stop: stdout=%q stderr=%q", n.out.String(), n.err.String())
	}
	n.cancel = nil
}

func inventoryBudgetMetrics(t *testing.T, node *inventoryBudgetNode) map[string]int64 {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + node.health + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var metrics map[string]int64
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&metrics))
	return metrics
}

func inventoryBudgetPrometheus(t *testing.T, node *inventoryBudgetNode) string {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + node.health + "/metrics/prometheus")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func TestRun_budgetedInventoryConvergesAfterTargetRestart(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	var dataSets [][]byte
	for i := 0; i < 8; i++ {
		dataSets = append(dataSets, []byte(fmt.Sprintf("streamhive-budgeted-inventory-blob-%02d", i)))
	}
	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	keys := make([][]byte, 0, len(dataSets))
	for _, data := range dataSets {
		key := storage.SHA256Key(data)
		require.NoError(t, sourceStore.Put(ctx, key, data))
		keys = append(keys, key)
	}

	source := startInventoryBudgetNode(t, sourceDir, "")
	t.Cleanup(func() { source.stop(t) })
	target := startInventoryBudgetNode(t, targetDir, source.listen)
	t.Cleanup(func() { target.stop(t) })

	var pending map[string]int64
	require.Eventually(t, func() bool {
		metrics, requestErr := tryInventoryBudgetMetrics(source)
		if requestErr != nil {
			return false
		}
		pending = metrics
		return metrics["replication_inventory_exchanges_limited"] >= 1 &&
			metrics["replication_inventory_exchanges_active"] >= 1
	}, 3*time.Second, 10*time.Millisecond, "source metrics=%v source stderr=%q target stderr=%q", pending, source.err.String(), target.err.String())

	target.stop(t)
	require.Eventually(t, func() bool {
		metrics := inventoryBudgetMetrics(t, source)
		return metrics["replication_inventory_exchanges_active"] == 0
	}, 3*time.Second, 10*time.Millisecond, "source did not forget disconnected target: metrics=%v", inventoryBudgetMetrics(t, source))

	target = startInventoryBudgetNode(t, targetDir, source.listen)
	require.Eventually(t, func() bool {
		for _, key := range keys {
			store, storeErr := storage.NewFileStore(targetDir)
			if storeErr != nil {
				return false
			}
			if data, getErr := store.Get(ctx, key); getErr != nil || len(data) == 0 {
				return false
			}
		}
		metrics := inventoryBudgetMetrics(t, source)
		return metrics["replication_inventory_exchanges_started"] >= 2 &&
			metrics["replication_inventory_exchanges_completed"] >= 1 &&
			metrics["replication_inventory_exchanges_active"] == 0
	}, 8*time.Second, 20*time.Millisecond, "source metrics=%v source stderr=%q target stderr=%q", inventoryBudgetMetrics(t, source), source.err.String(), target.err.String())

	targetStore, err := storage.NewFileStore(targetDir)
	require.NoError(t, err)
	gotKeys, err := targetStore.ListKeys(ctx)
	require.NoError(t, err)
	require.Len(t, gotKeys, len(keys))
	for _, key := range keys {
		data, err := targetStore.Get(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, key, storage.SHA256Key(data))
	}

	metrics := inventoryBudgetMetrics(t, source)
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_limited"], int64(1))
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_completed"], int64(1))
	assert.GreaterOrEqual(t, metrics["replication_inventory_keys_sent"], int64(len(keys)))
	assert.Equal(t, int64(0), metrics["replication_inventory_exchanges_active"])
	assert.Equal(t, int64(0), metrics["replication_inventory_exchanges_dropped"])

	prometheus := inventoryBudgetPrometheus(t, source)
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_exchanges_limited %d\n", metrics["replication_inventory_exchanges_limited"]))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_exchanges_completed %d\n", metrics["replication_inventory_exchanges_completed"]))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_keys_sent %d\n", metrics["replication_inventory_keys_sent"]))
	assert.Contains(t, prometheus, "streamhive_replication_inventory_exchanges_active 0\n")
}

func TestRun_localEvictionRehydratesThroughStartupOnlyRepair(t *testing.T) {
	ctx := context.Background()
	data := []byte("streamhive-local-eviction-startup-repair")
	key := storage.SHA256Key(data)
	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	require.NoError(t, sourceStore.Put(ctx, key, data))

	source := startInventoryBudgetNode(t, sourceDir, "")
	t.Cleanup(func() { source.stop(t) })
	target := startInventoryBudgetNode(t, targetDir, source.listen)
	t.Cleanup(func() { target.stop(t) })

	require.Eventually(t, func() bool {
		store, storeErr := storage.NewFileStore(targetDir)
		if storeErr != nil {
			return false
		}
		got, getErr := store.Get(ctx, key)
		if getErr != nil || string(got) != string(data) {
			return false
		}
		metrics, metricsErr := tryInventoryBudgetMetrics(source)
		return metricsErr == nil &&
			metrics["replication_inventory_exchanges_completed"] >= 1 &&
			metrics["replication_inventory_exchanges_active"] == 0
	}, 5*time.Second, 20*time.Millisecond, "initial repair did not converge: source metrics=%v source stderr=%q target stderr=%q", inventoryBudgetMetrics(t, source), source.err.String(), target.err.String())

	target.stop(t)
	targetStore, err := storage.NewFileStore(targetDir)
	require.NoError(t, err)
	require.NoError(t, targetStore.Delete(ctx, key))
	hasKey, err := targetStore.Has(ctx, key)
	require.NoError(t, err)
	require.False(t, hasKey, "target eviction should remove only the local blob")
	localKeys, err := targetStore.ListKeys(ctx)
	require.NoError(t, err)
	require.Empty(t, localKeys, "target should start the repair with an empty local object set")

	target = startInventoryBudgetNode(t, targetDir, source.listen)
	require.Eventually(t, func() bool {
		store, storeErr := storage.NewFileStore(targetDir)
		if storeErr != nil {
			return false
		}
		got, getErr := store.Get(ctx, key)
		if getErr != nil || string(got) != string(data) {
			return false
		}
		metrics, metricsErr := tryInventoryBudgetMetrics(source)
		return metricsErr == nil &&
			metrics["replication_inventory_exchanges_started"] >= 2 &&
			metrics["replication_inventory_exchanges_completed"] >= 2 &&
			metrics["replication_inventory_exchanges_active"] == 0
	}, 5*time.Second, 20*time.Millisecond, "startup-only repair did not converge: source metrics=%v source stderr=%q target stderr=%q", inventoryBudgetMetrics(t, source), source.err.String(), target.err.String())

	finalStore, err := storage.NewFileStore(targetDir)
	require.NoError(t, err)
	repaired, err := finalStore.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, key, storage.SHA256Key(repaired))
	assert.Equal(t, data, repaired)

	metrics := inventoryBudgetMetrics(t, source)
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_started"], int64(2))
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_completed"], int64(2))
	assert.GreaterOrEqual(t, metrics["replication_inventory_keys_sent"], int64(2))
	assert.Equal(t, int64(0), metrics["replication_inventory_exchanges_active"])
	assert.Equal(t, int64(0), metrics["replication_inventory_exchanges_dropped"])
}

func tryInventoryBudgetMetrics(source *inventoryBudgetNode) (map[string]int64, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + source.health + "/metrics")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics status: %s", resp.Status)
	}
	var metrics map[string]int64
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return nil, err
	}
	return metrics, nil
}
