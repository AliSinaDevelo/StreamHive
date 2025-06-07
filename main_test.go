package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// safeBuffer is an io.Writer safe for concurrent writes and reads from another goroutine (e.g. with require.Eventually).
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

	cancel()
	<-errCh
}
