package p2p

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mtlsAcceptanceCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

type mtlsAcceptanceMaterial struct {
	serverName          string
	roots               *x509.CertPool
	serverCert          tls.Certificate
	clientCert          tls.Certificate
	untrustedClientCert tls.Certificate
}

func newMTLSAcceptanceMaterial(t *testing.T) mtlsAcceptanceMaterial {
	t.Helper()
	trustedCA := newMTLSAcceptanceCA(t, "StreamHive mTLS acceptance CA", 1)
	untrustedCA := newMTLSAcceptanceCA(t, "StreamHive unrelated CA", 2)
	roots := x509.NewCertPool()
	roots.AddCert(trustedCA.cert)
	return mtlsAcceptanceMaterial{
		serverName:          "streamhive.test",
		roots:               roots,
		serverCert:          newMTLSAcceptanceCert(t, trustedCA, "streamhive.test", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 3),
		clientCert:          newMTLSAcceptanceCert(t, trustedCA, "streamhive-client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, 4),
		untrustedClientCert: newMTLSAcceptanceCert(t, untrustedCA, "untrusted-client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, 5),
	}
}

func newMTLSAcceptanceCA(t *testing.T, commonName string, serial int64) mtlsAcceptanceCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return mtlsAcceptanceCA{cert: cert, key: key}
}

func newMTLSAcceptanceCert(t *testing.T, ca mtlsAcceptanceCA, commonName string, usages []x509.ExtKeyUsage, serial int64) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serialNumber(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"streamhive.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return cert
}

func serialNumber(value int64) *big.Int {
	return big.NewInt(value)
}

func mtlsServerConfig(material mtlsAcceptanceMaterial) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{material.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    material.roots,
		MinVersion:   tls.VersionTLS12,
	}
}

func mtlsClientConfig(material mtlsAcceptanceMaterial, cert *tls.Certificate) *tls.Config {
	config := &tls.Config{
		RootCAs:    material.roots,
		ServerName: material.serverName,
		MinVersion: tls.VersionTLS12,
	}
	if cert != nil {
		config.Certificates = []tls.Certificate{*cert}
	}
	return config
}

func newMTLSAcceptanceTransport(address string) *TCPTransport {
	transport := NewTCPTransport(address)
	transport.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	transport.TLSHandshakeTimeout = time.Second
	return transport
}

func TestTCPTransport_mutualTLSAdmitsVerifiedClient(t *testing.T) {
	ctx := context.Background()
	material := newMTLSAcceptanceMaterial(t)
	server := newMTLSAcceptanceTransport("127.0.0.1:0")
	server.TLSServerConfig = mtlsServerConfig(material)
	var serverSeen atomic.Int32
	var received atomic.Value
	server.OnPeer = func(Peer) { serverSeen.Add(1) }
	server.FrameHandler = func(_ context.Context, _ Peer, payload []byte) error {
		received.Store(append([]byte(nil), payload...))
		return nil
	}
	require.NoError(t, server.ListenAndAccept(ctx))
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	client := newMTLSAcceptanceTransport("127.0.0.1:0")
	client.TLSClientConfig = mtlsClientConfig(material, &material.clientCert)
	var clientSeen atomic.Int32
	writeErr := make(chan error, 1)
	client.OnPeer = func(peer Peer) {
		clientSeen.Add(1)
		tcpPeer, ok := peer.(*TCPPeer)
		if !ok {
			writeErr <- assert.AnError
			return
		}
		writeErr <- tcpPeer.WriteFrame([]byte("mTLS-ping"), DefaultMaxFrameBytes)
	}
	require.NoError(t, client.ListenAndAccept(ctx))
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.NoError(t, client.Dial(ctx, server.Addr().String()))
	require.NoError(t, <-writeErr)
	waitFor(t, func() bool {
		return serverSeen.Load() == 1 && clientSeen.Load() == 1 && received.Load() != nil
	})

	assert.Equal(t, []byte("mTLS-ping"), received.Load().([]byte))
	assert.Equal(t, uint64(1), server.Metrics().TLSHandshakeSuccess.Load())
	assert.Equal(t, uint64(0), server.Metrics().TLSHandshakeFailures.Load())
	assert.Equal(t, uint64(1), client.Metrics().TLSHandshakeSuccess.Load())
	assert.Equal(t, uint64(0), client.Metrics().TLSHandshakeFailures.Load())
	assert.Equal(t, int64(1), server.Metrics().ActivePeers.Load())
	assert.Equal(t, uint64(1), server.Metrics().FramesHandled.Load())
	assert.Equal(t, int64(1), server.Metrics().Snapshot()["active_peers"])
	assert.Equal(t, int64(1), server.Metrics().Snapshot()["tls_handshake_success"])
}

func TestTCPTransport_mutualTLSRejectsUntrustedClientBeforeRegistration(t *testing.T) {
	material := newMTLSAcceptanceMaterial(t)
	for _, test := range []struct {
		name string
		cert *tls.Certificate
	}{
		{name: "missing-client-certificate"},
		{name: "untrusted-client-certificate", cert: &material.untrustedClientCert},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			server := newMTLSAcceptanceTransport("127.0.0.1:0")
			server.TLSServerConfig = mtlsServerConfig(material)
			var serverSeen, handled atomic.Int32
			server.OnPeer = func(Peer) { serverSeen.Add(1) }
			server.FrameHandler = func(context.Context, Peer, []byte) error {
				handled.Add(1)
				return nil
			}
			require.NoError(t, server.ListenAndAccept(ctx))
			t.Cleanup(func() { require.NoError(t, server.Close()) })

			client := newMTLSAcceptanceTransport("127.0.0.1:0")
			client.TLSClientConfig = mtlsClientConfig(material, test.cert)
			var clientDisconnected atomic.Int32
			client.OnPeerDisconnected = func(Peer) { clientDisconnected.Add(1) }
			t.Cleanup(func() { require.NoError(t, client.Close()) })

			_ = client.Dial(ctx, server.Addr().String())
			waitFor(t, func() bool { return server.Metrics().TLSHandshakeFailures.Load() == 1 })
			waitFor(t, func() bool { return clientDisconnected.Load() == 1 })

			assert.Equal(t, int32(0), serverSeen.Load())
			assert.Equal(t, int32(0), handled.Load())
			assert.Empty(t, server.Peers())
			assert.Equal(t, int64(0), server.Metrics().ActivePeers.Load())
			assert.Equal(t, uint64(0), server.Metrics().TLSHandshakeSuccess.Load())
			assert.Equal(t, uint64(1), server.Metrics().TLSHandshakeFailures.Load())
			assert.Equal(t, uint64(1), client.Metrics().TLSHandshakeSuccess.Load())
			assert.Equal(t, uint64(0), client.Metrics().TLSHandshakeFailures.Load())
			assert.Equal(t, uint64(0), client.Metrics().DialErrors.Load())
			assert.Equal(t, uint64(1), client.Metrics().DialSuccess.Load())
			assert.Empty(t, client.Peers())
		})
	}
}
