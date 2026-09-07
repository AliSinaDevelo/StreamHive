package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
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

type tlsAcceptanceMaterial struct {
	caPath         string
	serverCertPath string
	serverKeyPath  string
	caCert         *x509.Certificate
	caKey          *ecdsa.PrivateKey
}

func newTLSAcceptanceMaterial(t *testing.T) tlsAcceptanceMaterial {
	t.Helper()

	dir := t.TempDir()
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "StreamHive acceptance CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "streamhive.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"streamhive.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	require.NoError(t, err)

	material := tlsAcceptanceMaterial{
		caPath:         filepath.Join(dir, "ca.pem"),
		serverCertPath: filepath.Join(dir, "server-cert.pem"),
		serverKeyPath:  filepath.Join(dir, "server-key.pem"),
		caCert:         caTemplate,
		caKey:          caKey,
	}
	require.NoError(t, os.WriteFile(material.caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644))
	require.NoError(t, os.WriteFile(material.serverCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o644))
	require.NoError(t, os.WriteFile(material.serverKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), 0o600))
	return material
}

func writeTLSAcceptanceServerPair(t *testing.T, material tlsAcceptanceMaterial, serial int64) (string, string) {
	t.Helper()

	now := time.Now().UTC()
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "streamhive.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"streamhive.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, material.caCert, &serverKey.PublicKey, material.caKey)
	require.NoError(t, err)
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	require.NoError(t, err)

	dir := filepath.Dir(material.serverCertPath)
	certPath := filepath.Join(dir, fmt.Sprintf("server-%d-cert.pem", serial))
	keyPath := filepath.Join(dir, fmt.Sprintf("server-%d-key.pem", serial))
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o644))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), 0o600))
	return certPath, keyPath
}

func writeTLSAcceptanceClientPair(t *testing.T, material tlsAcceptanceMaterial, serial int64) (string, string) {
	t.Helper()

	now := time.Now().UTC()
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "streamhive-client"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, material.caCert, &clientKey.PublicKey, material.caKey)
	require.NoError(t, err)
	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	require.NoError(t, err)

	dir := filepath.Dir(material.serverCertPath)
	certPath := filepath.Join(dir, fmt.Sprintf("client-%d-cert.pem", serial))
	keyPath := filepath.Join(dir, fmt.Sprintf("client-%d-key.pem", serial))
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), 0o644))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKeyDER}), 0o600))
	return certPath, keyPath
}

func startTLSAcceptanceNode(t *testing.T, args ...string) *inventoryBudgetNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	node := &inventoryBudgetNode{
		cancel: cancel,
		errCh:  make(chan error, 1),
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

func TestRun_tlsPeerAuthReplicatesContentBlob(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	serverDir := t.TempDir()
	const token = "tls-acceptance-shared-token"

	server := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", serverDir,
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-peer-auth-token", token,
		"-peer-id", "server",
		"-peer-allow-ids", "client",
	)
	t.Cleanup(func() { server.stop(t) })

	clientCtx, clientCancel := context.WithCancel(context.Background())
	var clientOut, clientErr safeBuffer
	clientErrCh := make(chan error, 1)
	clientDone := make(chan struct{})
	data := []byte("streamhive tls authenticated content")
	key := storage.SHA256Key(data)
	go func() {
		defer close(clientDone)
		clientErrCh <- run(clientCtx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", server.listen,
			"-replicate",
			"-tls-ca", material.caPath,
			"-tls-server-name", "streamhive.test",
			"-peer-auth-token", token,
			"-peer-id", "client",
			"-put-content-key",
			"-put-data", string(data),
		}, &clientOut, &clientErr)
	}()
	t.Cleanup(func() {
		clientCancel()
		select {
		case <-clientDone:
		case <-time.After(5 * time.Second):
			t.Errorf("TLS client did not stop: stdout=%q stderr=%q", clientOut.String(), clientErr.String())
		}
	})

	require.Eventually(t, func() bool {
		store, err := storage.NewFileStore(serverDir)
		if err != nil {
			return false
		}
		got, err := store.Get(context.Background(), key)
		return err == nil && string(got) == string(data)
	}, 5*time.Second, 20*time.Millisecond, "server stderr=%q client stderr=%q", server.err.String(), clientErr.String())
	require.Eventually(t, func() bool {
		log := server.err.String()
		return strings.Contains(log, "auth_method=shared-token") &&
			strings.Contains(log, "auth_identity=client")
	}, 5*time.Second, 20*time.Millisecond, "server stderr=%q", server.err.String())

	var metrics map[string]int64
	var metricsErr error
	require.Eventually(t, func() bool {
		metrics, metricsErr = tryInventoryBudgetMetrics(server)
		return metricsErr == nil &&
			metrics["peer_auth_success"] >= 1 &&
			metrics["replication_blobs_stored"] >= 1
	}, 5*time.Second, 20*time.Millisecond, "metrics=%v metrics_err=%v server stderr=%q", metrics, metricsErr, server.err.String())
	assert.GreaterOrEqual(t, metrics["peer_auth_success"], int64(1))
	assert.GreaterOrEqual(t, metrics["replication_blobs_stored"], int64(1))
	prometheus := inventoryBudgetPrometheus(t, server)
	assert.Contains(t, prometheus, "streamhive_peer_auth_success ")
	assert.Contains(t, prometheus, "streamhive_replication_blobs_stored ")

	clientCancel()
	require.NoError(t, <-clientErrCh, "client stdout=%q stderr=%q", clientOut.String(), clientErr.String())
}

func TestRun_tlsVerificationRejectsBeforePeerAdmission(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	wrongCA := newTLSAcceptanceMaterial(t)
	const token = "tls-negative-shared-token"

	tests := []struct {
		name       string
		caPath     string
		serverName string
	}{
		{name: "wrong-ca", caPath: wrongCA.caPath, serverName: "streamhive.test"},
		{name: "wrong-server-name", caPath: material.caPath, serverName: "wrong.streamhive.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := startTLSAcceptanceNode(t,
				"-listen", "127.0.0.1:0",
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

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var clientOut, clientErr safeBuffer
			err := run(ctx, []string{
				"-listen", "127.0.0.1:0",
				"-dial", server.listen,
				"-replicate",
				"-tls-ca", tt.caPath,
				"-tls-server-name", tt.serverName,
				"-peer-auth-token", token,
				"-peer-id", "client",
				"-put-content-key",
				"-put-data", "must not cross failed tls admission",
				"-exit-after-put",
			}, &clientOut, &clientErr)
			require.Error(t, err, "client stdout=%q stderr=%q", clientOut.String(), clientErr.String())
			assert.Contains(t, strings.ToLower(err.Error()), "tls")

			require.Eventually(t, func() bool {
				metrics, requestErr := tryInventoryBudgetMetrics(server)
				return requestErr == nil && metrics["active_peers"] == 0 && metrics["peer_auth_success"] == 0
			}, 3*time.Second, 20*time.Millisecond, "server metrics=%v stderr=%q", inventoryBudgetMetrics(t, server), server.err.String())
		})
	}
}
