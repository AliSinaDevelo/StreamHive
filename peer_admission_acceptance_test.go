package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_maxPeersRejectsSecondRealTCPPeer(t *testing.T) {
	server := startInventoryBudgetNodeWithConfig(t, t.TempDir(), "", "0s", 1)
	t.Cleanup(func() { server.stop(t) })
	first := startInventoryBudgetNode(t, t.TempDir(), server.listen)
	t.Cleanup(func() { first.stop(t) })
	second := startInventoryBudgetNode(t, t.TempDir(), server.listen)
	t.Cleanup(func() { second.stop(t) })

	var serverMetrics, firstMetrics, secondMetrics map[string]int64
	require.Eventually(t, func() bool {
		var err error
		serverMetrics, err = tryInventoryBudgetMetrics(server)
		if err != nil {
			return false
		}
		firstMetrics, err = tryInventoryBudgetMetrics(first)
		if err != nil {
			return false
		}
		secondMetrics, err = tryInventoryBudgetMetrics(second)
		if err != nil {
			return false
		}
		return serverMetrics["active_peers"] == 1 &&
			serverMetrics["peers_rejected"] >= 1 &&
			firstMetrics["active_peers"]+secondMetrics["active_peers"] == 1
	}, 3*time.Second, 20*time.Millisecond, "server metrics=%v first metrics=%v second metrics=%v", serverMetrics, firstMetrics, secondMetrics)

	assert.Equal(t, int64(1), serverMetrics["active_peers"])
	assert.GreaterOrEqual(t, serverMetrics["peers_rejected"], int64(1))
	assert.Equal(t, int64(1), firstMetrics["active_peers"]+secondMetrics["active_peers"])

	prometheus := inventoryBudgetPrometheus(t, server)
	assert.Contains(t, prometheus, "streamhive_active_peers 1\n")
	assert.Contains(t, prometheus, "streamhive_peers_rejected ")

	// The admitted peer remains usable after the second connection is rejected.
	admitted := first
	if firstMetrics["active_peers"] == 0 {
		admitted = second
	}
	require.Eventually(t, func() bool {
		metrics, err := tryInventoryBudgetMetrics(admitted)
		return err == nil && metrics["active_peers"] == 1
	}, time.Second, 20*time.Millisecond, "admitted peer was disturbed: metrics=%v", serverMetrics)

}
