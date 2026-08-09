package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_peerReconnectHealthMetrics(t *testing.T) {
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	targetAddr := targetListener.Addr().String()
	require.NoError(t, targetListener.Close())

	ctx, cancel := context.WithCancel(context.Background())
	node := &inventoryBudgetNode{
		cancel: cancel,
		errCh:  make(chan error, 1),
	}
	go func() {
		node.errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-health", "127.0.0.1:0",
			"-peers", targetAddr,
			"-peer-reconnect",
			"-peer-reconnect-min", "10ms",
			"-peer-reconnect-max", "40ms",
		}, &node.out, &node.err)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(node.out.String(), "listening on") && strings.Contains(node.err.String(), "msg=health")
	}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", node.out.String(), node.err.String())
	healthMatch := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`).FindStringSubmatch(node.err.String())
	require.Len(t, healthMatch, 2, "stderr=%q", node.err.String())
	node.health = healthMatch[1]
	t.Cleanup(func() { node.stop(t) })

	var retryMetrics map[string]int64
	require.Eventually(t, func() bool {
		metrics, metricsErr := tryInventoryBudgetMetrics(node)
		if metricsErr != nil {
			return false
		}
		retryMetrics = metrics
		return metrics["peer_reconnect_targets"] == 1 &&
			metrics["peer_reconnect_active"] == 1 &&
			metrics["peer_reconnect_attempts"] > 0 &&
			metrics["peer_reconnect_failures"] > 0
	}, 3*time.Second, 10*time.Millisecond, "retry metrics=%v stderr=%q", retryMetrics, node.err.String())

	server := p2p.NewTCPTransport(targetAddr)
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		serverCancel()
		_ = server.Close()
	})
	require.NoError(t, server.ListenAndAccept(serverCtx))

	var connectedMetrics map[string]int64
	require.Eventually(t, func() bool {
		metrics, metricsErr := tryInventoryBudgetMetrics(node)
		if metricsErr != nil {
			return false
		}
		connectedMetrics = metrics
		return metrics["active_peers"] == 1 &&
			metrics["peer_reconnect_successes"] >= 1 &&
			metrics["peer_reconnect_active"] == 0
	}, 3*time.Second, 10*time.Millisecond, "connected metrics=%v stderr=%q", connectedMetrics, node.err.String())

	prometheus := inventoryBudgetPrometheus(t, node)
	assert.Contains(t, prometheus, "streamhive_peer_reconnect_targets 1\n")
	assert.Contains(t, prometheus, "streamhive_peer_reconnect_failures")
	assert.Contains(t, prometheus, "streamhive_peer_reconnect_successes")
	assert.NotContains(t, prometheus, "remote=")
}
