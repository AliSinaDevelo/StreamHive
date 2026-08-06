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
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTLSCredentialPair(t *testing.T, material tlsAcceptanceMaterial, name string, serial int64, notBefore, notAfter time.Time, usage x509.ExtKeyUsage) (string, string, *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	if usage == x509.ExtKeyUsageServerAuth {
		template.DNSNames = []string{"streamhive.test"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, material.caCert, &key.PublicKey, material.caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	dir := filepath.Dir(material.serverCertPath)
	certPath := filepath.Join(dir, fmt.Sprintf("%s-%d-cert.pem", name, serial))
	keyPath := filepath.Join(dir, fmt.Sprintf("%s-%d-key.pem", name, serial))
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath, template
}

func getReadyStatus(t *testing.T, node *inventoryBudgetNode) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + node.health + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestRun_tlsCredentialHealthReportsAggregateStatus(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	now := time.Now().UTC()
	clientCertPath, clientKeyPath, clientCert := writeTLSCredentialPair(
		t,
		material,
		"streamhive-client-short-lived",
		8,
		now.Add(-time.Minute),
		now.Add(10*time.Minute),
		x509.ExtKeyUsageClientAuth,
	)

	node := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-tls-client-cert", clientCertPath,
		"-tls-client-key", clientKeyPath,
		"-tls-expiry-warning", "2h",
	)
	t.Cleanup(func() { node.stop(t) })

	metrics := inventoryBudgetMetrics(t, node)
	assert.Equal(t, int64(2), metrics["tls_certificates_configured"])
	assert.Equal(t, clientCert.NotAfter.Unix(), metrics["tls_certificate_expiry_timestamp_seconds"])
	assert.Zero(t, metrics["tls_certificates_expired"])
	assert.Zero(t, metrics["tls_certificates_not_yet_valid"])
	assert.Equal(t, int64(2), metrics["tls_certificates_expiring_soon"])
	assert.Equal(t, http.StatusOK, getReadyStatus(t, node))

	prometheus := inventoryBudgetPrometheus(t, node)
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_tls_certificates_configured 2\n"))
	assert.Contains(t, prometheus, fmt.Sprintf("streamhive_tls_certificate_expiry_timestamp_seconds %d\n", clientCert.NotAfter.Unix()))
	assert.NotContains(t, prometheus, "remote=")
	assert.NotContains(t, prometheus, "serial")
	assert.NotContains(t, prometheus, "subject")
}

func TestRun_tlsCredentialHealthCanDisableWarningWindow(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	now := time.Now().UTC()
	clientCertPath, clientKeyPath, _ := writeTLSCredentialPair(
		t,
		material,
		"streamhive-client-warning-disabled",
		9,
		now.Add(-time.Minute),
		now.Add(time.Minute),
		x509.ExtKeyUsageClientAuth,
	)

	node := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-tls-client-cert", clientCertPath,
		"-tls-client-key", clientKeyPath,
		"-tls-expiry-warning", "0",
	)
	t.Cleanup(func() { node.stop(t) })

	metrics := inventoryBudgetMetrics(t, node)
	assert.Zero(t, metrics["tls_certificates_expiring_soon"])
	assert.Equal(t, http.StatusOK, getReadyStatus(t, node))
}

func TestRun_tlsCredentialHealthRejectsInvalidBeforeListen(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	now := time.Now().UTC()
	tests := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		wantError string
	}{
		{name: "expired", notBefore: now.Add(-2 * time.Hour), notAfter: now.Add(-time.Minute), wantError: "certificate expired"},
		{name: "not-yet-valid", notBefore: now.Add(time.Hour), notAfter: now.Add(2 * time.Hour), wantError: "certificate is not valid before"},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			certPath, keyPath, _ := writeTLSCredentialPair(t, material, test.name, int64(20+i), test.notBefore, test.notAfter, x509.ExtKeyUsageServerAuth)
			var out, stderr safeBuffer
			err := run(context.Background(), []string{
				"-listen", "127.0.0.1:0",
				"-tls-cert", certPath,
				"-tls-key", keyPath,
			}, &out, &stderr)
			require.Error(t, err, "stdout=%q stderr=%q", out.String(), stderr.String())
			assert.Contains(t, err.Error(), test.wantError)
			assert.Empty(t, out.String())
		})
	}
}

func TestTLSCredentialHealthSnapshotAndReadiness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	health := &tlsCredentialHealth{
		warningWindow: time.Hour,
		credentials: []tlsCredentialValidity{
			{notBefore: now.Add(-time.Hour), notAfter: now.Add(30 * time.Minute)},
			{notBefore: now.Add(10 * time.Minute), notAfter: now.Add(2 * time.Hour)},
			{notBefore: now.Add(-time.Hour), notAfter: now.Add(-time.Minute)},
		},
	}

	metrics := health.Snapshot(now)
	assert.Equal(t, int64(3), metrics["tls_certificates_configured"])
	assert.Equal(t, now.Add(-time.Minute).Unix(), metrics["tls_certificate_expiry_timestamp_seconds"])
	assert.Equal(t, int64(1), metrics["tls_certificates_expired"])
	assert.Equal(t, int64(1), metrics["tls_certificates_not_yet_valid"])
	assert.Equal(t, int64(1), metrics["tls_certificates_expiring_soon"])
	assert.False(t, health.Ready(now))

	validHealth := &tlsCredentialHealth{
		warningWindow: time.Hour,
		credentials:   []tlsCredentialValidity{{notBefore: now.Add(-time.Hour), notAfter: now.Add(30 * time.Minute)}},
	}
	assert.True(t, validHealth.Ready(now))
	assert.False(t, validHealth.Ready(now.Add(time.Hour)))
}
