package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_budgetedInventoryConvergesAcrossPeersAndSourceMutation(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetADir := t.TempDir()
	targetBDir := t.TempDir()

	sourceStore, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	initialKeys := make([][]byte, 0, 16)
	for i := 0; i < 16; i++ {
		data := []byte(fmt.Sprintf("streamhive-mutation-inventory-blob-%02d", i))
		key := storage.SHA256Key(data)
		require.NoError(t, sourceStore.Put(ctx, key, data))
		initialKeys = append(initialKeys, key)
	}
	sort.Slice(initialKeys, func(i, j int) bool { return bytes.Compare(initialKeys[i], initialKeys[j]) < 0 })

	// Delete the highest key while the first one-key page is pending. It cannot
	// have been advertised yet, so the add-only repair protocol can converge to
	// the final source set without a tombstone message.
	deletedKey := append([]byte(nil), initialKeys[len(initialKeys)-1]...)
	mutatedData := []byte("streamhive-mutation-inventory-blob-added-after-start")
	mutatedKey := storage.SHA256Key(mutatedData)
	for suffix := 0; bytes.Compare(mutatedKey, deletedKey) <= 0; suffix++ {
		mutatedData = []byte(fmt.Sprintf("streamhive-mutation-inventory-blob-added-after-start-%d", suffix))
		mutatedKey = storage.SHA256Key(mutatedData)
	}
	finalKeys := make([][]byte, 0, len(initialKeys))
	for _, key := range initialKeys {
		if !bytes.Equal(key, deletedKey) {
			finalKeys = append(finalKeys, key)
		}
	}
	finalKeys = append(finalKeys, mutatedKey)
	sort.Slice(finalKeys, func(i, j int) bool { return bytes.Compare(finalKeys[i], finalKeys[j]) < 0 })

	source := startInventoryBudgetNode(t, sourceDir, "")
	t.Cleanup(func() { source.stop(t) })
	targetA := startInventoryBudgetNode(t, targetADir, source.listen)
	t.Cleanup(func() { targetA.stop(t) })
	targetB := startInventoryBudgetNode(t, targetBDir, source.listen)
	t.Cleanup(func() { targetB.stop(t) })

	var pending map[string]int64
	require.Eventually(t, func() bool {
		metrics, requestErr := tryInventoryBudgetMetrics(source)
		if requestErr != nil {
			return false
		}
		pending = metrics
		return metrics["replication_inventory_exchanges_limited"] >= 2 &&
			metrics["replication_inventory_exchanges_active"] >= 2
	}, 3*time.Second, 10*time.Millisecond, "source metrics=%v source stderr=%q target-a stderr=%q target-b stderr=%q", pending, source.err.String(), targetA.err.String(), targetB.err.String())

	mutator, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	require.NoError(t, mutator.Delete(ctx, deletedKey))
	require.NoError(t, mutator.Put(ctx, mutatedKey, mutatedData))

	targetA.stop(t)
	require.Eventually(t, func() bool {
		metrics := inventoryBudgetMetrics(t, source)
		return metrics["replication_inventory_exchanges_active"] == 1
	}, 3*time.Second, 10*time.Millisecond, "source did not retain target-b exchange: metrics=%v", inventoryBudgetMetrics(t, source))

	require.Eventually(t, func() bool {
		return inventoryBudgetStoreHasExactKeys(targetBDir, finalKeys)
	}, 8*time.Second, 20*time.Millisecond, "target-b did not converge: source metrics=%v", inventoryBudgetMetrics(t, source))

	targetA = startInventoryBudgetNode(t, targetADir, source.listen)
	require.Eventually(t, func() bool {
		if !inventoryBudgetStoreHasExactKeys(targetADir, finalKeys) {
			return false
		}
		metrics := inventoryBudgetMetrics(t, source)
		return metrics["replication_inventory_exchanges_completed"] >= 2 &&
			metrics["replication_inventory_exchanges_active"] == 0
	}, 8*time.Second, 20*time.Millisecond, "source metrics=%v target-a stderr=%q target-b stderr=%q", inventoryBudgetMetrics(t, source), targetA.err.String(), targetB.err.String())

	sourceFinal, err := storage.NewFileStore(sourceDir)
	require.NoError(t, err)
	assert.True(t, inventoryBudgetStoreHasExactKeys(sourceDir, finalKeys))
	assert.False(t, inventoryBudgetStoreHasExactKeys(sourceDir, append(finalKeys, deletedKey)))
	for _, key := range finalKeys {
		data, err := sourceFinal.Get(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, key, storage.SHA256Key(data))
	}

	metrics := inventoryBudgetMetrics(t, source)
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_limited"], int64(2))
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_completed"], int64(2))
	assert.GreaterOrEqual(t, metrics["replication_inventory_keys_sent"], int64(len(finalKeys)*2))
	assert.GreaterOrEqual(t, metrics["replication_inventory_exchanges_dropped"], int64(0))
	assert.Equal(t, int64(0), metrics["replication_inventory_exchanges_active"])

	prometheus := inventoryBudgetPrometheus(t, source)
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_exchanges_limited %d\n", metrics["replication_inventory_exchanges_limited"]))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_exchanges_completed %d\n", metrics["replication_inventory_exchanges_completed"]))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_keys_sent %d\n", metrics["replication_inventory_keys_sent"]))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_replication_inventory_exchanges_dropped %d\n", metrics["replication_inventory_exchanges_dropped"]))
	assert.Contains(t, prometheus, "streamhive_replication_inventory_exchanges_active 0\n")
}

func inventoryBudgetStoreHasExactKeys(dir string, expected [][]byte) bool {
	store, err := storage.NewFileStore(dir)
	if err != nil {
		return false
	}
	got, err := store.ListKeys(context.Background())
	if err != nil || len(got) != len(expected) {
		return false
	}
	for i, key := range expected {
		if !bytes.Equal(got[i], key) {
			return false
		}
		if _, err := store.Get(context.Background(), key); err != nil {
			return false
		}
	}
	return true
}
