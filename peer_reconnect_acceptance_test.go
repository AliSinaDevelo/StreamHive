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

func TestRun_peerReconnectFastDisconnectContinues(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	targetAddr := listener.Addr().String()
	accepted := make(chan net.Conn, 1)
	var acceptedConn net.Conn
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for connectionNumber := 1; ; connectionNumber++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if connectionNumber == 1 {
				if tcpConn, ok := conn.(*net.TCPConn); ok {
					_ = tcpConn.SetLinger(0)
				}
				_ = conn.Close()
				continue
			}
			accepted <- conn
			return
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if acceptedConn != nil {
			_ = acceptedConn.Close()
		} else {
			select {
			case conn := <-accepted:
				_ = conn.Close()
			default:
			}
		}
		<-serverDone
	})

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
	t.Cleanup(func() { node.stop(t) })

	require.Eventually(t, func() bool {
		return strings.Contains(node.out.String(), "listening on") && strings.Contains(node.err.String(), "msg=health")
	}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", node.out.String(), node.err.String())
	node.health = regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`).FindStringSubmatch(node.err.String())[1]

	var metrics map[string]int64
	select {
	case acceptedConn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatalf("target did not accept a retry: metrics=%v stderr=%q", metrics, node.err.String())
	}

	require.Eventually(t, func() bool {
		var metricsErr error
		metrics, metricsErr = tryInventoryBudgetMetrics(node)
		return metricsErr == nil &&
			metrics["peer_reconnect_attempts"] >= 2 &&
			metrics["peer_reconnect_successes"] >= 1 &&
			metrics["peer_reconnect_active"] == 0
	}, 5*time.Second, 10*time.Millisecond, "metrics=%v stderr=%q", metrics, node.err.String())
}

func TestRun_peerReconnectHostnameAfterDisconnect(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	targetAddr := reserved.Addr().String()
	_, port, err := net.SplitHostPort(targetAddr)
	require.NoError(t, err)
	require.NoError(t, reserved.Close())
	target := net.JoinHostPort("localhost", port)

	ctx, cancel := context.WithCancel(context.Background())
	node := &inventoryBudgetNode{
		cancel: cancel,
		errCh:  make(chan error, 1),
	}
	go func() {
		node.errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-health", "127.0.0.1:0",
			"-peers", target,
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
			metrics["peer_reconnect_attempts"] > 0 &&
			metrics["peer_reconnect_failures"] > 0
	}, 3*time.Second, 10*time.Millisecond, "retry metrics=%v stderr=%q", retryMetrics, node.err.String())

	serverCtx, serverCancel := context.WithCancel(context.Background())
	server := p2p.NewTCPTransport(targetAddr)
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, server.ListenAndAccept(serverCtx))
	t.Cleanup(func() {
		serverCancel()
		_ = server.Close()
	})

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

	firstSuccesses := connectedMetrics["peer_reconnect_successes"]
	serverCancel()
	require.NoError(t, server.Close())

	var disconnectedMetrics map[string]int64
	require.Eventually(t, func() bool {
		metrics, metricsErr := tryInventoryBudgetMetrics(node)
		if metricsErr != nil {
			return false
		}
		disconnectedMetrics = metrics
		return metrics["active_peers"] == 0 &&
			metrics["peer_reconnect_failures"] > retryMetrics["peer_reconnect_failures"] &&
			metrics["peer_reconnect_active"] == 1
	}, 3*time.Second, 10*time.Millisecond, "disconnect retry metrics=%v stderr=%q", disconnectedMetrics, node.err.String())

	serverCtx, serverCancel = context.WithCancel(context.Background())
	server = p2p.NewTCPTransport(targetAddr)
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, server.ListenAndAccept(serverCtx))
	t.Cleanup(func() {
		serverCancel()
		_ = server.Close()
	})

	var recoveredMetrics map[string]int64
	require.Eventually(t, func() bool {
		metrics, metricsErr := tryInventoryBudgetMetrics(node)
		if metricsErr != nil {
			return false
		}
		recoveredMetrics = metrics
		return metrics["active_peers"] == 1 &&
			metrics["peer_reconnect_successes"] >= firstSuccesses+1 &&
			metrics["peer_reconnect_active"] == 0
	}, 3*time.Second, 10*time.Millisecond, "recovered metrics=%v stderr=%q", recoveredMetrics, node.err.String())

	prometheus := inventoryBudgetPrometheus(t, node)
	assert.Contains(t, prometheus, "streamhive_peer_reconnect_targets 1\n")
	assert.NotContains(t, prometheus, "remote=")
}
