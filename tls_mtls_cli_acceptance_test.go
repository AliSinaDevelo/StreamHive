package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForCLIMTLSAdmission(t *testing.T, node *inventoryBudgetNode) {
	t.Helper()

	require.Eventually(t, func() bool {
		metrics, err := tryInventoryBudgetMetrics(node)
		return err == nil &&
			metrics["active_peers"] == 1 &&
			metrics["tls_handshake_success"] >= 1 &&
			metrics["peer_auth_success"] >= 1
	}, 5*time.Second, 20*time.Millisecond, "metrics=%v stderr=%q", tryMetricsForTLSRotation(node), node.err.String())
}

func TestRun_tlsMutualTLSClientCertificateAdmission(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	clientCertPath, clientKeyPath := writeTLSAcceptanceClientPair(t, material, 4)
	const token = "tls-cli-mtls-shared-token"

	server := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-tls-cert", material.serverCertPath,
		"-tls-key", material.serverKeyPath,
		"-tls-client-ca", material.caPath,
		"-tls-require-client-cert",
		"-peer-auth-token", token,
		"-peer-id", "server",
		"-peer-allow-ids", "client",
	)
	t.Cleanup(func() { server.stop(t) })

	client := startTLSAcceptanceNode(t,
		"-listen", "127.0.0.1:0",
		"-dial", server.listen,
		"-health", "127.0.0.1:0",
		"-replicate",
		"-store-dir", t.TempDir(),
		"-tls-ca", material.caPath,
		"-tls-server-name", "streamhive.test",
		"-tls-client-cert", clientCertPath,
		"-tls-client-key", clientKeyPath,
		"-peer-auth-token", token,
		"-peer-id", "client",
	)
	t.Cleanup(func() { client.stop(t) })

	waitForCLIMTLSAdmission(t, server)
	prometheus := inventoryBudgetPrometheus(t, server)
	assert.Contains(t, prometheus, "streamhive_tls_handshake_success ")
	assert.NotContains(t, prometheus, "cert_")
	assert.NotContains(t, prometheus, "secret")
	assert.NotContains(t, prometheus, "remote=")
}

func TestRun_tlsMutualTLSRejectsBeforePeerAdmission(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	wrongMaterial := newTLSAcceptanceMaterial(t)
	wrongClientCertPath, wrongClientKeyPath := writeTLSAcceptanceClientPair(t, wrongMaterial, 6)
	const token = "tls-cli-mtls-negative-token"

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing-client-certificate"},
		{name: "untrusted-client-certificate", args: []string{"-tls-client-cert", wrongClientCertPath, "-tls-client-key", wrongClientKeyPath}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := startTLSAcceptanceNode(t,
				"-listen", "127.0.0.1:0",
				"-health", "127.0.0.1:0",
				"-replicate",
				"-store-dir", t.TempDir(),
				"-tls-cert", material.serverCertPath,
				"-tls-key", material.serverKeyPath,
				"-tls-client-ca", material.caPath,
				"-tls-require-client-cert",
				"-peer-auth-token", token,
				"-peer-id", "server",
				"-peer-allow-ids", "client",
			)
			t.Cleanup(func() { server.stop(t) })

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var out, stderr safeBuffer
			args := []string{
				"-listen", "127.0.0.1:0",
				"-dial", server.listen,
				"-tls-ca", material.caPath,
				"-tls-server-name", "streamhive.test",
				"-peer-auth-token", token,
				"-peer-id", "client",
			}
			args = append(args, test.args...)
			err := run(ctx, args, &out, &stderr)
			require.Error(t, err, "stdout=%q stderr=%q", out.String(), stderr.String())
			assert.NotEmpty(t, err.Error())

			require.Eventually(t, func() bool {
				metrics, metricsErr := tryInventoryBudgetMetrics(server)
				return metricsErr == nil &&
					metrics["active_peers"] == 0 &&
					metrics["peer_auth_success"] == 0 &&
					metrics["tls_handshake_failures"] >= 1
			}, 3*time.Second, 20*time.Millisecond, "metrics=%v stderr=%q", tryMetricsForTLSRotation(server), server.err.String())
			assert.Contains(t, out.String(), "listening on")
		})
	}
}

func TestRun_tlsMutualTLSFlagsFailClosed(t *testing.T) {
	material := newTLSAcceptanceMaterial(t)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "require-client-cert-needs-client-ca",
			args: []string{
				"-tls-cert", material.serverCertPath,
				"-tls-key", material.serverKeyPath,
				"-tls-require-client-cert",
			},
			want: "requires -tls-client-ca",
		},
		{
			name: "client-ca-needs-explicit-requirement",
			args: []string{
				"-tls-cert", material.serverCertPath,
				"-tls-key", material.serverKeyPath,
				"-tls-client-ca", material.caPath,
			},
			want: "requires -tls-require-client-cert",
		},
		{
			name: "client-cert-needs-client-key",
			args: []string{
				"-tls-client-cert", material.serverCertPath,
			},
			want: "both -tls-client-cert and -tls-client-key",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr safeBuffer
			err := run(context.Background(), append([]string{"-listen", "127.0.0.1:0"}, test.args...), &out, &stderr)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
			assert.Empty(t, out.String())
		})
	}
}
