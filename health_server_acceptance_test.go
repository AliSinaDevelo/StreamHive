package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startHealthAcceptanceNode(t *testing.T) *inventoryBudgetNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	node := &inventoryBudgetNode{
		cancel: cancel,
		errCh:  make(chan error, 1),
	}
	go func() {
		node.errCh <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-health", "127.0.0.1:0",
		}, &node.out, &node.err)
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

func TestRun_healthServerRejectsOversizedHeader(t *testing.T) {
	node := startHealthAcceptanceNode(t)
	t.Cleanup(func() { node.stop(t) })

	conn, err := net.DialTimeout("tcp", node.health, time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))

	request := "GET /livez HTTP/1.1\r\nHost: " + node.health + "\r\nX-Fill: " + strings.Repeat("x", defaultHealthMaxHeaderBytes+16*1024) + "\r\n\r\n"
	_, writeErr := conn.Write([]byte(request))
	statusLine, readErr := bufio.NewReader(conn).ReadString('\n')
	if writeErr == nil {
		require.NoError(t, readErr)
		assert.Contains(t, statusLine, "431")
	} else {
		assert.Error(t, writeErr)
	}
}

func TestRun_healthServerShutsDownWithContext(t *testing.T) {
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i+1), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var out, stderr safeBuffer
			errCh := make(chan error, 1)
			go func() {
				errCh <- run(ctx, []string{
					"-listen", "127.0.0.1:0",
					"-health", "127.0.0.1:0",
				}, &out, &stderr)
			}()

			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), "listening on") && strings.Contains(stderr.String(), "msg=health")
			}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())
			healthMatch := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`).FindStringSubmatch(stderr.String())
			require.Len(t, healthMatch, 2, "stderr=%q", stderr.String())

			resp, err := http.Get("http://" + healthMatch[1] + "/livez")
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())

			cancel()
			select {
			case runErr := <-errCh:
				require.NoError(t, runErr)
			case <-time.After(5 * time.Second):
				t.Fatal("run did not stop")
			}

			require.Eventually(t, func() bool {
				client := &http.Client{Timeout: 100 * time.Millisecond}
				response, requestErr := client.Get("http://" + healthMatch[1] + "/livez")
				if response != nil {
					_ = response.Body.Close()
				}
				return requestErr != nil
			}, 2*time.Second, 20*time.Millisecond, "health listener still serving: %q", stderr.String())
		})
	}
}
