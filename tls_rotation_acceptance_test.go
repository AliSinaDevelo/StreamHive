package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reserveTLSRotationAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func replaceTLSRotationFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	tmp := path + ".next"
	require.NoError(t, os.WriteFile(tmp, data, mode))
	require.NoError(t, os.Rename(tmp, path))
}

func waitForTLSRotationAdmission(t *testing.T, node *inventoryBudgetNode) {
	t.Helper()

	require.Eventually(t, func() bool {
		metrics, err := tryInventoryBudgetMetrics(node)
		return err == nil &&
			metrics["active_peers"] == 1 &&
			metrics["tls_handshake_success"] >= 1 &&
			metrics["peer_auth_success"] >= 1
	}, 5*time.Second, 20*time.Millisecond, "metrics=%v stderr=%q", tryMetricsForTLSRotation(node), node.err.String())
}

func tlsRotationServerSerial(t *testing.T, addr, caPath string) string {
	t.Helper()

	caPEM, err := os.ReadFile(caPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "streamhive.test",
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	require.NotEmpty(t, state.PeerCertificates)
	return state.PeerCertificates[0].SerialNumber.String()
}

func tryMetricsForTLSRotation(node *inventoryBudgetNode) map[string]int64 {
	metrics, err := tryInventoryBudgetMetrics(node)
	if err != nil {
		return map[string]int64{"error": 1}
	}
	return metrics
}

func TestRun_tlsRestartRotationReconnectsStaticPeer(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	rotatedCertPath, rotatedKeyPath := writeTLSAcceptanceServerPair(t, material, 3)
	oldCert, err := os.ReadFile(material.serverCertPath)
	require.NoError(t, err)
	oldKey, err := os.ReadFile(material.serverKeyPath)
	require.NoError(t, err)
	newCert, err := os.ReadFile(rotatedCertPath)
	require.NoError(t, err)
	newKey, err := os.ReadFile(rotatedKeyPath)
	require.NoError(t, err)
	listenAddr := reserveTLSRotationAddress(t)
	const token = "tls-rotation-shared-token"

	server := startTLSAcceptanceNode(t,
		"-listen", listenAddr,
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-peer-auth-token", token,
		"-peer-id", "server",
		"-peer-allow-ids", "client",
	)
	t.Cleanup(func() { server.stop(t) })

	client := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-peers", listenAddr,
		"-peer-reconnect",
		"-peer-reconnect-min", "10ms",
		"-peer-reconnect-max", "100ms",
		"-tls-ca", material.caPath,
		"-tls-server-name", "streamhive.test",
		"-peer-auth-token", token,
		"-peer-id", "client",
	)
	t.Cleanup(func() { client.stop(t) })
	waitForTLSRotationAdmission(t, server)
	require.Equal(t, "2", tlsRotationServerSerial(t, listenAddr, material.caPath))

	server.stop(t)
	require.Eventually(t, func() bool {
		metrics, err := tryInventoryBudgetMetrics(client)
		return err == nil && metrics["active_peers"] == 0
	}, 3*time.Second, 20*time.Millisecond, "client metrics=%v stderr=%q", tryMetricsForTLSRotation(client), client.err.String())

	replaceTLSRotationFile(t, material.serverCertPath, newCert, 0o644)
	replaceTLSRotationFile(t, material.serverKeyPath, newKey, 0o600)
	server = startTLSAcceptanceNode(t,
		"-listen", listenAddr,
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-peer-auth-token", token,
		"-peer-id", "server",
		"-peer-allow-ids", "client",
	)
	t.Cleanup(func() { server.stop(t) })
	waitForTLSRotationAdmission(t, server)
	require.Eventually(t, func() bool {
		return strings.Contains(client.err.String(), "peer reconnect established")
	}, 2*time.Second, 20*time.Millisecond, "client stderr=%q", client.err.String())
	require.Equal(t, "3", tlsRotationServerSerial(t, listenAddr, material.caPath))

	server.stop(t)
	require.Eventually(t, func() bool {
		metrics, err := tryInventoryBudgetMetrics(client)
		return err == nil && metrics["active_peers"] == 0
	}, 3*time.Second, 20*time.Millisecond, "client metrics=%v stderr=%q", tryMetricsForTLSRotation(client), client.err.String())

	badCert := []byte("not a certificate")
	replaceTLSRotationFile(t, material.serverCertPath, badCert, 0o644)
	var badOut, badErr safeBuffer
	badCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	badRunErr := run(badCtx, []string{
		"-listen", listenAddr,
		"-health", "127.0.0.1:0",
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
	}, &badOut, &badErr)
	cancel()
	require.Error(t, badRunErr)
	assert.Contains(t, badRunErr.Error(), "tls: load server cert")
	assert.NotContains(t, badOut.String(), "listening on")

	replaceTLSRotationFile(t, material.serverCertPath, oldCert, 0o644)
	replaceTLSRotationFile(t, material.serverKeyPath, oldKey, 0o600)
	server = startTLSAcceptanceNode(t,
		"-listen", listenAddr,
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-peer-auth-token", token,
		"-peer-id", "server",
		"-peer-allow-ids", "client",
	)
	t.Cleanup(func() { server.stop(t) })
	waitForTLSRotationAdmission(t, server)
	require.Equal(t, "2", tlsRotationServerSerial(t, listenAddr, material.caPath))

	prometheus := inventoryBudgetPrometheus(t, server)
	assert.NotContains(t, prometheus, "cert_")
	assert.NotContains(t, prometheus, "secret")
	assert.NotContains(t, prometheus, "serial")
	assert.NotContains(t, prometheus, "remote=")
	assert.NotContains(t, prometheus, "address=")
}

func TestRun_tlsRestartRotationRejectsMalformedCertificateBeforeListen(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	listenAddr := reserveTLSRotationAddress(t)
	replaceTLSRotationFile(t, material.serverCertPath, []byte("bad certificate"), 0o644)

	var out, stderr safeBuffer
	err := run(context.Background(), []string{
		"-listen", listenAddr,
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
	}, &out, &stderr)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "tls: load server cert")
	assert.Empty(t, out.String())
}
