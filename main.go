package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/internal/version"
	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
)

const (
	defaultPutAckTimeout           = time.Second
	defaultPutRetries              = 2
	defaultPutRetryDelay           = 100 * time.Millisecond
	maxPutRetries                  = 10
	maxPutRetryDelay               = 500 * time.Millisecond
	defaultHealthReadHeaderTimeout = 5 * time.Second
	defaultHealthReadTimeout       = 10 * time.Second
	defaultHealthWriteTimeout      = 10 * time.Second
	defaultHealthIdleTimeout       = 60 * time.Second
	defaultHealthMaxHeaderBytes    = 1 << 20
	defaultShutdownGrace           = 3 * time.Second
	repairContinuationDelay        = 100 * time.Millisecond
	maxRepairContinuationAttempts  = 1
	defaultMaxRepairOps            = 4
	defaultTLSExpiryWarning        = 30 * 24 * time.Hour
	maxDuration                    = time.Duration(1<<63 - 1)
)

var errBlobAckTimeout = errors.New("replication: timed out waiting for blob acknowledgment")

const (
	blobOutcomeAccepted   = "accepted"
	blobOutcomeAckTimeout = "ack-timeout"
	blobOutcomeWriteError = "write-error"
	blobOutcomeCanceled   = "canceled"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("streamhive", flag.ContinueOnError)
	fs.SetOutput(stderr)

	listen := fs.String("listen", "127.0.0.1:0", "TCP listen address")
	dial := fs.String("dial", "", "optional peer host:port to dial after listen")
	peers := fs.String("peers", "", "comma-separated peer host:port list to dial after listen")
	peerReconnect := fs.Bool("peer-reconnect", false, "keep retrying -peers with exponential backoff")
	peerReconnectMin := fs.Duration("peer-reconnect-min", 500*time.Millisecond, "minimum reconnect backoff for -peer-reconnect")
	peerReconnectMax := fs.Duration("peer-reconnect-max", 30*time.Second, "maximum reconnect backoff for -peer-reconnect")
	syncInterval := fs.Duration("sync-interval", 0, "periodically advertise local blob keys to connected peers (0 = startup only)")
	health := fs.String("health", "", "optional HTTP listen addr for /livez /readyz /peers /metrics (e.g. :8080)")
	shutdownGrace := fs.Duration("shutdown-grace", defaultShutdownGrace, "bounded graceful shutdown deadline for health and P2P drain")
	maxPeers := fs.Int("max-peers", 0, "max simultaneous peers (0 = unlimited)")
	peerAuthToken := fs.String("peer-auth-token", "", "optional shared token required before peer registration")
	peerAuthTimeout := fs.Duration("peer-auth-timeout", p2p.DefaultPeerAuthTimeout, "timeout for optional peer auth handshake")
	peerID := fs.String("peer-id", "", "optional application identity sent during shared-token auth")
	peerAllowIDs := fs.String("peer-allow-ids", "", "comma-separated inbound application identities allowed during shared-token auth")
	dialTimeout := fs.Duration("dial-timeout", 0, "default dial timeout (0 = use context only)")
	readIdle := fs.Duration("read-idle-timeout", 0, "TCP read deadline refresh for peer loops (0 = none for discard mode)")
	showVer := fs.Bool("version", false, "print version and exit")
	replicate := fs.Bool("replicate", false, "enable in-memory blob replication from framed peers")
	storeDir := fs.String("store-dir", "", "directory for durable replicated blobs (requires -replicate)")
	listKeys := fs.Bool("list-keys", false, "print durable store keys as hex and exit (requires -store-dir)")
	putKey := fs.String("put-key", "", "send one replicated blob key to -dial peer")
	putData := fs.String("put-data", "", "send one replicated blob value to -dial peer")
	putContentKey := fs.Bool("put-content-key", false, "derive the replicated blob key from SHA-256(-put-data)")
	exitAfterPut := fs.Bool("exit-after-put", false, "exit after sending one blob to outbound peers")
	putAckTimeout := fs.Duration("put-ack-timeout", defaultPutAckTimeout, "time to wait for each blob acknowledgment")
	putRetries := fs.Int("put-retries", defaultPutRetries, "additional blob sends after an acknowledgment timeout")
	putRetryDelay := fs.Duration("put-retry-delay", defaultPutRetryDelay, "delay before retrying a blob after an acknowledgment timeout")
	maxBlobBytes := fs.Int("max-blob-bytes", replication.DefaultMaxDataBytes, "max replicated blob payload bytes")
	maxRepairBytes := fs.Int("max-repair-bytes", replication.DefaultMaxRepairBytes, "max aggregate anti-entropy blob data bytes per request (0 = default)")
	maxRepairOps := fs.Int("max-repair-ops", defaultMaxRepairOps, "max concurrent anti-entropy blob reads/writes (0 = default)")
	maxInventoryBytes := fs.Int("max-inventory-bytes", defaultMaxInventoryBytes, "max encoded anti-entropy inventory bytes per peer exchange (0 = unlimited)")
	maxInventoryKeys := fs.Int("max-inventory-keys", defaultMaxInventoryKeys, "max anti-entropy inventory keys per peer exchange (0 = unlimited)")
	lifecycleEnabled := fs.Bool("lifecycle", false, "enable opt-in v0.13 lifecycle state and repair capability")
	lifecycleDir := fs.String("lifecycle-dir", "", "directory for lifecycle journal, checkpoint, and peer watermarks (requires -lifecycle)")
	lifecycleMaxRecords := fs.Int("lifecycle-max-records", lifecycle.DefaultMaxRepairRecords, "max lifecycle records per repair frame")
	lifecycleMaxKeyBytes := fs.Int("lifecycle-max-key-bytes", lifecycle.DefaultMaxRepairLogicalKeyBytes, "max lifecycle logical-key bytes per repair frame")
	lifecycleMaxMetadataBytes := fs.Int("lifecycle-max-metadata-bytes", lifecycle.DefaultMaxRepairMetadataBytes, "max lifecycle metadata bytes per repair frame")
	lifecycleMaxFrameBytes := fs.Int("lifecycle-max-frame-bytes", lifecycle.DefaultMaxRepairFrameBytes, "max encoded lifecycle repair frame bytes")
	lifecycleMembersFlag := fs.String("lifecycle-members", "", "comma-separated operator-fenced lifecycle replica identities (requires -lifecycle; an explicit empty value creates an empty fence)")
	lifecycleCompact := fs.Bool("lifecycle-compact", false, "write a verified lifecycle checkpoint through the durable tail and exit")
	lifecyclePutNamespace := fs.String("lifecycle-put-namespace", "", "create one lifecycle present record in this namespace")
	lifecyclePutKey := fs.String("lifecycle-put-key", "", "logical key for one lifecycle present record")
	lifecyclePutData := fs.String("lifecycle-put-data", "", "data for one lifecycle present record")
	lifecyclePutBlobKey := fs.String("lifecycle-put-blob-key", "", "optional hex SHA-256 key to validate for lifecycle put data")
	lifecycleDeleteNamespace := fs.String("lifecycle-delete-namespace", "", "create one lifecycle tombstone in this namespace")
	lifecycleDeleteKey := fs.String("lifecycle-delete-key", "", "logical key for one lifecycle tombstone")
	lifecycleExitAfterMutation := fs.Bool("lifecycle-exit-after-mutation", false, "wait for lifecycle acknowledgements, then exit after one local mutation")
	lifecycleMutationTimeout := fs.Duration("lifecycle-mutation-timeout", 10*time.Second, "bounded wait for lifecycle mutation acknowledgements")

	tlsCert := fs.String("tls-cert", "", "path to PEM certificate (enables TLS on listener)")
	tlsKey := fs.String("tls-key", "", "path to PEM private key for -tls-cert")
	tlsCA := fs.String("tls-ca", "", "optional path to PEM CA bundle for outbound TLS")
	tlsServerName := fs.String("tls-server-name", "", "SNI / cert verification name for outbound TLS")
	tlsClientCert := fs.String("tls-client-cert", "", "path to PEM client certificate for outbound mTLS")
	tlsClientKey := fs.String("tls-client-key", "", "path to PEM private key for -tls-client-cert")
	tlsClientCA := fs.String("tls-client-ca", "", "path to PEM CA bundle for verifying inbound client certificates")
	tlsRequireClientCert := fs.Bool("tls-require-client-cert", false, "require and verify client certificates on the TLS listener")
	tlsExpiryWarning := fs.Duration("tls-expiry-warning", defaultTLSExpiryWarning, "warning window for configured TLS identities (0 = disabled)")
	insecureSkip := fs.Bool("tls-insecure-skip-verify", false, "skip TLS verify on outbound (dev only)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *showVer {
		_, err := fmt.Fprintln(stdout, version.Version)
		return err
	}
	dialTarget := strings.TrimSpace(*dial)
	peerList, err := parsePeerList(*peers)
	if err != nil {
		return err
	}
	peerAllowedIDs, err := parsePeerIdentityList(*peerAllowIDs)
	if err != nil {
		return err
	}
	lifecycleMembers, err := parsePeerIdentityList(*lifecycleMembersFlag)
	if err != nil {
		return fmt.Errorf("lifecycle: invalid membership list: %w", err)
	}
	lifecycleMembersConfigured := flagWasSet(fs, "lifecycle-members")
	peerTargets := combinePeerTargets(dialTarget, peerList)
	putRequested := *putKey != "" || *putContentKey
	lifecyclePutRequested := *lifecyclePutNamespace != "" || *lifecyclePutKey != "" || *lifecyclePutData != "" || *lifecyclePutBlobKey != ""
	lifecycleDeleteRequested := *lifecycleDeleteNamespace != "" || *lifecycleDeleteKey != ""
	lifecycleMutationRequested := lifecyclePutRequested || lifecycleDeleteRequested
	if putRequested && len(peerTargets) == 0 {
		return fmt.Errorf("replication: -put-key or -put-content-key requires -dial or -peers")
	}
	if *putContentKey && *putKey != "" {
		return fmt.Errorf("replication: -put-content-key cannot be combined with -put-key")
	}
	if lifecyclePutRequested && lifecycleDeleteRequested {
		return fmt.Errorf("lifecycle: put and delete commands are mutually exclusive")
	}
	if lifecycleMutationRequested && putRequested {
		return fmt.Errorf("lifecycle: local mutation cannot be combined with raw blob put")
	}
	if lifecycleMutationRequested && !*lifecycleEnabled {
		return fmt.Errorf("lifecycle: local mutation requires -lifecycle")
	}
	if *lifecycleCompact && !*lifecycleEnabled {
		return fmt.Errorf("lifecycle: -lifecycle-compact requires -lifecycle")
	}
	if *lifecycleCompact && lifecycleMutationRequested {
		return fmt.Errorf("lifecycle: compaction cannot be combined with a local mutation")
	}
	if *lifecycleCompact && putRequested {
		return fmt.Errorf("lifecycle: compaction cannot be combined with raw blob put")
	}
	if *lifecycleCompact && len(peerTargets) > 0 {
		return fmt.Errorf("lifecycle: compaction cannot be combined with -dial or -peers")
	}
	if *lifecycleCompact && *peerReconnect {
		return fmt.Errorf("lifecycle: compaction cannot be combined with -peer-reconnect")
	}
	if *lifecycleCompact && *lifecycleExitAfterMutation {
		return fmt.Errorf("lifecycle: compaction cannot be combined with -lifecycle-exit-after-mutation")
	}
	if lifecyclePutRequested && (*lifecyclePutNamespace == "" || *lifecyclePutKey == "") {
		return fmt.Errorf("lifecycle: put requires -lifecycle-put-namespace and -lifecycle-put-key")
	}
	if lifecycleDeleteRequested && (*lifecycleDeleteNamespace == "" || *lifecycleDeleteKey == "") {
		return fmt.Errorf("lifecycle: delete requires -lifecycle-delete-namespace and -lifecycle-delete-key")
	}
	if *lifecycleExitAfterMutation && !lifecycleMutationRequested {
		return fmt.Errorf("lifecycle: -lifecycle-exit-after-mutation requires a local mutation")
	}
	if *lifecycleMutationTimeout <= 0 {
		return fmt.Errorf("lifecycle: -lifecycle-mutation-timeout must be greater than zero")
	}
	if *peerReconnect {
		if len(peerList) == 0 {
			return fmt.Errorf("peers: -peer-reconnect requires -peers")
		}
		if putRequested {
			return fmt.Errorf("replication: -peer-reconnect cannot be combined with -put-key or -put-content-key")
		}
		if lifecycleMutationRequested {
			return fmt.Errorf("replication: -peer-reconnect cannot be combined with a lifecycle mutation")
		}
		if err := validateReconnectBackoff(*peerReconnectMin, *peerReconnectMax); err != nil {
			return err
		}
	}
	if *listKeys {
		if *storeDir == "" {
			return fmt.Errorf("storage: -list-keys requires -store-dir")
		}
		store, err := storage.NewFileStore(*storeDir)
		if err != nil {
			return fmt.Errorf("storage: open file store: %w", err)
		}
		keys, err := store.ListKeys(ctx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if _, err := fmt.Fprintln(stdout, hex.EncodeToString(key)); err != nil {
				return err
			}
		}
		return nil
	}
	if *storeDir != "" && !*replicate {
		return fmt.Errorf("storage: -store-dir requires -replicate")
	}
	if *syncInterval < 0 {
		return fmt.Errorf("replication: -sync-interval must be zero or greater")
	}
	if *shutdownGrace <= 0 {
		return fmt.Errorf("shutdown: -shutdown-grace must be greater than zero")
	}
	if *peerAuthTimeout < 0 {
		return fmt.Errorf("peers: -peer-auth-timeout must be zero or greater")
	}
	if *peerID != "" && *peerAuthToken == "" {
		return fmt.Errorf("peers: -peer-id requires -peer-auth-token")
	}
	if len(peerAllowedIDs) > 0 && *peerAuthToken == "" {
		return fmt.Errorf("peers: -peer-allow-ids requires -peer-auth-token")
	}
	if *putAckTimeout <= 0 {
		return fmt.Errorf("replication: -put-ack-timeout must be greater than zero")
	}
	if *putRetries < 0 || *putRetries > maxPutRetries {
		return fmt.Errorf("replication: -put-retries must be between 0 and %d", maxPutRetries)
	}
	if *putRetryDelay < 0 {
		return fmt.Errorf("replication: -put-retry-delay must be zero or greater")
	}
	if *maxRepairBytes < 0 {
		return fmt.Errorf("replication: -max-repair-bytes must be zero or greater")
	}
	if *maxRepairOps < 0 {
		return fmt.Errorf("replication: -max-repair-ops must be zero or greater")
	}
	if *maxInventoryBytes < 0 {
		return fmt.Errorf("replication: -max-inventory-bytes must be zero or greater")
	}
	if *maxInventoryKeys < 0 {
		return fmt.Errorf("replication: -max-inventory-keys must be zero or greater")
	}
	if *lifecycleMaxRecords < 0 || *lifecycleMaxKeyBytes < 0 || *lifecycleMaxMetadataBytes < 0 || *lifecycleMaxFrameBytes < 0 {
		return fmt.Errorf("lifecycle: repair limits must be zero or greater")
	}
	var lifecyclePutBlobKeyBytes []byte
	if *lifecyclePutBlobKey != "" {
		lifecyclePutBlobKeyBytes, err = storage.ParseSHA256KeyHex(*lifecyclePutBlobKey)
		if err != nil {
			return fmt.Errorf("lifecycle: invalid -lifecycle-put-blob-key: %w", err)
		}
	}
	if *tlsExpiryWarning < 0 {
		return fmt.Errorf("tls: -tls-expiry-warning must be zero or greater")
	}

	replLimits := replication.Limits{MaxDataBytes: *maxBlobBytes, MaxRepairBytes: *maxRepairBytes}
	inventoryBudget := inventoryExchangeBudget{maxBytes: *maxInventoryBytes, maxKeys: *maxInventoryKeys}
	var blobStore storage.BlobStore
	var keyLister storage.BlobKeyLister
	var memoryStore *storage.MemoryStore
	replMetrics := &replicationMetrics{}
	replMetrics.repairBudget = newRepairIOLimiter(*maxRepairOps, replMetrics)
	if *replicate {
		if *storeDir != "" {
			var err error
			blobStore, err = storage.NewFileStore(*storeDir)
			if err != nil {
				return fmt.Errorf("storage: open file store: %w", err)
			}
		} else {
			memoryStore = storage.NewMemoryStore()
			blobStore = memoryStore
		}
		if lister, ok := blobStore.(storage.BlobKeyLister); ok {
			keyLister = lister
		}
	}
	var putPayload []byte
	var putKeyBytes []byte
	var putKeyLabel string
	var putResult chan error
	var putTracker *putAckTracker
	if putRequested {
		putKeyBytes, putKeyLabel = resolvePutKey(*putKey, []byte(*putData), *putContentKey)
		putPayload, err = replication.EncodeBlobPut(putKeyBytes, []byte(*putData), replLimits)
		if err != nil {
			return err
		}
		putResult = make(chan error, len(peerTargets))
		putTracker = newPutAckTracker(replMetrics)
		replMetrics.ackTracker = putTracker
	}
	if *lifecycleEnabled && putTracker == nil {
		putTracker = newPutAckTracker(replMetrics)
		replMetrics.ackTracker = putTracker
	}
	lifecycleConfig := lifecycleCLIConfig{
		enabled:              *lifecycleEnabled,
		dir:                  *lifecycleDir,
		membershipConfigured: lifecycleMembersConfigured,
		membershipMembers:    lifecycleMembers,
		repairLimits: lifecycle.RepairLimits{
			MaxRecords:         *lifecycleMaxRecords,
			MaxLogicalKeyBytes: *lifecycleMaxKeyBytes,
			MaxMetadataBytes:   *lifecycleMaxMetadataBytes,
			MaxFrameBytes:      *lifecycleMaxFrameBytes,
		},
	}
	if err := lifecycleConfig.validate(blobStore, *peerAuthToken, *peerID); err != nil {
		return err
	}
	lifecycleState, err := openLifecycleRuntime(ctx, lifecycleConfig, blobStore, *peerAuthToken, *peerID)
	if err != nil {
		return err
	}
	defer func() {
		if err := lifecycleState.Close(); err != nil {
			log.Warn("lifecycle journal close", "err", err)
		}
	}()
	if *lifecycleCompact {
		if err := lifecycleState.compact(ctx); err != nil {
			return fmt.Errorf("lifecycle: compact: %w", err)
		}
		watermark := lifecycleState.journal.Floor()
		_, err := fmt.Fprintf(stdout, "lifecycle compacted watermark=%d/%d members=%d\n", watermark.Epoch, watermark.Sequence, len(lifecycleState.membership.Snapshot()))
		return err
	}

	tr := p2p.NewTCPTransport(*listen)
	tr.Logger = log
	tr.MaxPeers = *maxPeers
	tr.PeerAuthToken = *peerAuthToken
	tr.PeerAuthTimeout = *peerAuthTimeout
	tr.PeerAuthIdentity = *peerID
	tr.PeerAuthAllowedIdentities = peerAllowedIDs
	if lifecycleState != nil {
		tr.PeerAuthCapabilities = []string{
			p2p.CapabilityLifecycleV1,
			p2p.CapabilityLifecycleRepairReconcileV1,
		}
	}
	tr.DialTimeout = *dialTimeout
	tr.ReadIdleTimeout = *readIdle
	var inventoryScheduler *inventoryExchangeScheduler
	if keyLister != nil {
		inventoryScheduler = newInventoryExchangeScheduler(ctx, keyLister, replLimits, tr.MaxFrameBytes, inventoryBudget, replMetrics, log, repairContinuationDelay)
	}
	var lifecycleRawSync lifecycleRawRecordSync
	if lifecycleState != nil {
		lifecycleRawSync = func(syncCtx context.Context, peer p2p.Peer, records []lifecycle.Record) error {
			return syncLifecycleRawRecords(syncCtx, peer, lifecycleState.blobs, records, replLimits, tr.MaxFrameBytes, putTracker, *putAckTimeout, *putRetries, *putRetryDelay, replMetrics, log)
		}
	}
	tr.OnPeer = func(peer p2p.Peer) {
		if ctx.Err() != nil {
			_ = peer.Close()
			return
		}
		log.Info("peer", "remote", peer.RemoteAddr().String(), "outbound", peer.IsOutbound(), "auth_method", authMethodForPeer(peer), "auth_identity", authIdentityForPeer(peer))
		if putPayload != nil && peer.IsOutbound() {
			go func() {
				err := sendBlobWithAck(
					ctx,
					peer,
					putPayload,
					putKeyBytes,
					len(*putData),
					tr.MaxFrameBytes,
					putTracker,
					*putAckTimeout,
					*putRetries,
					*putRetryDelay,
					replMetrics,
					log,
				)
				if err != nil {
					outcome := "failed"
					attempts := 0
					var deliveryErr *blobDeliveryError
					if errors.As(err, &deliveryErr) {
						outcome = deliveryErr.kind
						attempts = deliveryErr.attempts
					}
					attrs := []any{"remote", peer.RemoteAddr().String(), "key", putKeyLabel, "err", err, "outcome", outcome}
					if attempts > 0 {
						attrs = append(attrs, "attempts", attempts)
					}
					log.Error("replication send", attrs...)
				}
				reportPutResult(putResult, err)
			}()
		}
		if inventoryScheduler != nil {
			inventoryScheduler.Start(peer)
		}
		if lifecycleState != nil {
			lifecycleState.AttachPeer(ctx, peer, tr.MaxFrameBytes, log, lifecycleRawSync)
		}
	}
	var repairScheduler *repairContinuationScheduler
	if blobStore != nil {
		repairScheduler = newRepairContinuationScheduler(ctx, blobStore, replLimits, tr.MaxFrameBytes, replMetrics, log, repairContinuationDelay)
		replMetrics.repairScheduler = repairScheduler
	}
	if blobStore != nil || putTracker != nil {
		tr.FrameHandler = func(frameCtx context.Context, peer p2p.Peer, payload []byte) error {
			if lifecycleState != nil && isLifecycleRepairPayload(payload) {
				return lifecycleState.HandleFrame(frameCtx, peer, payload)
			}
			msg, err := replication.Decode(payload, replLimits)
			if err != nil {
				replMetrics.ApplyErrors.Add(1)
				return err
			}
			if blobStore == nil && msg.Type != replication.MessageTypeBlobAck {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := handleReplicationMessage(frameCtx, peer, blobStore, keyLister, msg, replLimits, tr.MaxFrameBytes, replMetrics, log, memoryStore); err != nil {
				replMetrics.ApplyErrors.Add(1)
				return err
			}
			return nil
		}
	}
	var reconnector *peerReconnector
	if *peerReconnect {
		reconnector = newPeerReconnector(ctx, tr, peerList, *peerReconnectMin, *peerReconnectMax, log)
	}
	if lifecycleState != nil || repairScheduler != nil || reconnector != nil {
		tr.OnPeerDisconnected = func(peer p2p.Peer) {
			if inventoryScheduler != nil {
				inventoryScheduler.Forget(peer)
			}
			if lifecycleState != nil {
				lifecycleState.ForgetPeer(peer)
			}
			if repairScheduler != nil {
				repairScheduler.Forget(peer)
			}
			if reconnector != nil {
				reconnector.OnPeerDisconnected(peer)
			}
		}
	}

	if *tlsClientCert != "" || *tlsClientKey != "" {
		if *tlsClientCert == "" || *tlsClientKey == "" {
			return fmt.Errorf("tls: both -tls-client-cert and -tls-client-key are required")
		}
	}

	var clientCertificate *tls.Certificate
	if *tlsClientCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsClientCert, *tlsClientKey)
		if err != nil {
			return fmt.Errorf("tls: load client cert: %w", err)
		}
		clientCertificate = &cert
	}

	if *tlsClientCA != "" && !*tlsRequireClientCert {
		return fmt.Errorf("tls: -tls-client-ca requires -tls-require-client-cert")
	}
	if *tlsRequireClientCert && *tlsClientCA == "" {
		return fmt.Errorf("tls: -tls-require-client-cert requires -tls-client-ca")
	}
	if *tlsRequireClientCert && (*tlsCert == "" || *tlsKey == "") {
		return fmt.Errorf("tls: -tls-require-client-cert requires -tls-cert and -tls-key")
	}
	var clientCAPool *x509.CertPool
	if *tlsClientCA != "" {
		clientCAPool, err = loadTLSCAPool(*tlsClientCA, "-tls-client-ca")
		if err != nil {
			return err
		}
	}
	var outboundCAPool *x509.CertPool
	if *tlsCA != "" {
		outboundCAPool, err = loadTLSCAPool(*tlsCA, "-tls-ca")
		if err != nil {
			return err
		}
	}

	var serverCertificate *tls.Certificate
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return fmt.Errorf("tls: both -tls-cert and -tls-key are required")
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("tls: load server cert: %w", err)
		}
		serverCertificate = &cert
		serverConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		if *tlsRequireClientCert {
			serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
			serverConfig.ClientCAs = clientCAPool
		}
		tr.TLSServerConfig = serverConfig
	}

	if len(peerTargets) > 0 && (*tlsCA != "" || *insecureSkip || *tlsServerName != "" || clientCertificate != nil) {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if *insecureSkip {
			cfg.InsecureSkipVerify = true
		}
		if *tlsServerName != "" {
			cfg.ServerName = *tlsServerName
		}
		if outboundCAPool != nil {
			cfg.RootCAs = outboundCAPool
		}
		if clientCertificate != nil {
			cfg.Certificates = []tls.Certificate{*clientCertificate}
		}
		tr.TLSClientConfig = cfg
	}

	tlsHealth, err := newTLSCredentialHealth(
		time.Now().UTC(),
		*tlsExpiryWarning,
		tlsCredentialInput{flagName: "-tls-cert", certificate: serverCertificate},
		tlsCredentialInput{flagName: "-tls-client-cert", certificate: clientCertificate},
	)
	if err != nil {
		return err
	}

	var hsrv *http.Server
	var lifecycleMutationRecord lifecycle.Record
	lifecycleMutationCommitted := false
	gracefulShutdown := false
	requestShutdown := func() {
		gracefulShutdown = true
		if err := shutdownApplication(*shutdownGrace, hsrv, tr, log); err != nil {
			log.Warn("shutdown completed with errors", "err", err)
		}
	}

	if err := tr.ListenAndAccept(ctx); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		if !gracefulShutdown {
			_ = tr.Close()
		}
	}()
	if keyLister != nil && *syncInterval > 0 {
		startPeriodicBlobHas(ctx, tr, inventoryScheduler, *syncInterval)
	}

	addr := tr.Addr()
	if addr == nil {
		return errors.New("no listen address")
	}
	if _, err := fmt.Fprintf(stdout, "listening on %s\n", addr.String()); err != nil {
		return err
	}
	if lifecycleMutationRequested {
		var result lifecycle.ApplyResult
		if lifecyclePutRequested {
			lifecycleMutationRecord, result, err = lifecycleState.put(ctx, *lifecyclePutNamespace, *lifecyclePutKey, []byte(*lifecyclePutData), lifecyclePutBlobKeyBytes)
		} else {
			lifecycleMutationRecord, result, err = lifecycleState.delete(ctx, *lifecycleDeleteNamespace, *lifecycleDeleteKey)
		}
		if err != nil {
			return fmt.Errorf("lifecycle: local mutation: %w", err)
		}
		lifecycleMutationCommitted = true
		if _, err := fmt.Fprintf(stdout, "lifecycle mutation committed state=%s version=%d/%d outcome=%s\n", lifecycleMutationRecord.State, lifecycleMutationRecord.Version.Epoch, lifecycleMutationRecord.Version.Sequence, result.Outcome); err != nil {
			return err
		}
	}

	if dialTarget != "" {
		if err := tr.Dial(ctx, dialTarget); err != nil {
			return fmt.Errorf("dial %s: %w", dialTarget, err)
		}
	}
	if reconnector != nil {
		reconnector.Start()
	} else {
		for _, target := range peerList {
			if err := tr.Dial(ctx, target); err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
		}
	}
	if lifecycleMutationCommitted && *lifecycleExitAfterMutation {
		waitCtx, cancel := context.WithTimeout(ctx, *lifecycleMutationTimeout)
		waitErr := lifecycleState.waitForVersion(waitCtx, lifecycleMutationRecord.Version, len(peerTargets))
		cancel()
		if waitErr != nil {
			return fmt.Errorf("lifecycle: mutation acknowledgements: %w", waitErr)
		}
		requestShutdown()
		return nil
	}
	if *exitAfterPut && putResult != nil {
		waitTimeout := putWaitTimeout(*putAckTimeout, *putRetryDelay, *putRetries)
		for range peerTargets {
			select {
			case err := <-putResult:
				if ctx.Err() != nil {
					requestShutdown()
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					return ctx.Err()
				}
				if err != nil {
					return fmt.Errorf("replication: send blob: %w", err)
				}
			case <-ctx.Done():
				requestShutdown()
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				return ctx.Err()
			case <-time.After(waitTimeout):
				return fmt.Errorf("replication: timed out waiting for blob send")
			}
		}
		return nil
	}

	if *health != "" {
		var err error
		hsrv, err = startHealth(*health, tr, replMetrics, keyLister, tlsHealth, lifecycleState, log)
		if err != nil {
			return fmt.Errorf("health: %w", err)
		}
	}

	<-ctx.Done()
	requestShutdown()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

func shutdownApplication(grace time.Duration, hsrv *http.Server, tr *p2p.TCPTransport, log *slog.Logger) error {
	if grace <= 0 {
		return fmt.Errorf("shutdown: grace period must be greater than zero")
	}
	if log == nil {
		log = slog.Default()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	log.Info("shutdown started", "grace", grace, "health", hsrv != nil)

	var shutdownErr error
	if hsrv != nil {
		if err := hsrv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("health shutdown: %w", err))
		}
	}
	if err := tr.Drain(shutdownCtx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("transport drain: %w", err))
	}

	snapshot := tr.Metrics().Snapshot()
	log.Info("shutdown complete",
		"shutdown_state", snapshot["shutdown_state"],
		"shutdown_tracked_peers", snapshot["shutdown_tracked_peers"],
		"shutdown_tracked_goroutines", snapshot["shutdown_tracked_goroutines"],
	)
	return shutdownErr
}

func reportPutResult(ch chan<- error, err error) {
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

type peerAuthMethodProvider interface {
	AuthMethod() string
}

type peerAuthIdentityProvider interface {
	AuthIdentity() string
}

func authMethodForPeer(peer p2p.Peer) string {
	if provider, ok := peer.(peerAuthMethodProvider); ok {
		return provider.AuthMethod()
	}
	return p2p.PeerAuthMethodNone
}

func authIdentityForPeer(peer p2p.Peer) string {
	if provider, ok := peer.(peerAuthIdentityProvider); ok {
		return provider.AuthIdentity()
	}
	return ""
}

type putAckID struct {
	remote string
	key    string
}

type blobDeliveryError struct {
	kind     string
	attempts int
	err      error
}

func (e *blobDeliveryError) Error() string {
	return fmt.Sprintf("replication: blob delivery %s after %d attempt(s): %v", e.kind, e.attempts, e.err)
}

func (e *blobDeliveryError) Unwrap() error { return e.err }

type putAckTracker struct {
	mu      sync.Mutex
	pending map[putAckID]chan struct{}
	metrics *replicationMetrics
}

func newPutAckTracker(metrics *replicationMetrics) *putAckTracker {
	return &putAckTracker{
		pending: make(map[putAckID]chan struct{}),
		metrics: metrics,
	}
}

func (t *putAckTracker) register(peer p2p.Peer, key []byte) <-chan struct{} {
	ack := make(chan struct{})
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	t.pending[id] = ack
	if t.metrics != nil {
		t.metrics.BlobAcksPending.Add(1)
	}
	t.mu.Unlock()
	return ack
}

func (t *putAckTracker) ack(peer p2p.Peer, key []byte) bool {
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	ack, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
		if t.metrics != nil {
			t.metrics.BlobAcksPending.Add(-1)
			t.metrics.BlobAcksMatched.Add(1)
		}
		close(ack)
	}
	t.mu.Unlock()
	return ok
}

func (t *putAckTracker) remove(peer p2p.Peer, key []byte) bool {
	id := putAckID{remote: peer.RemoteAddr().String(), key: string(key)}
	t.mu.Lock()
	_, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
		if t.metrics != nil {
			t.metrics.BlobAcksPending.Add(-1)
		}
	}
	t.mu.Unlock()
	return ok
}

func sendBlobWithAck(
	ctx context.Context,
	peer p2p.Peer,
	payload []byte,
	key []byte,
	blobBytes int,
	maxFrameBytes int,
	tracker *putAckTracker,
	ackTimeout time.Duration,
	retries int,
	retryDelay time.Duration,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	if tracker == nil {
		return errors.New("replication: blob acknowledgment tracker is required")
	}
	for attempt := 0; attempt <= retries; attempt++ {
		ack := tracker.register(peer, key)
		if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
			tracker.remove(peer, key)
			metrics.SendErrors.Add(1)
			metrics.BlobWriteErrors.Add(1)
			metrics.BlobPutFailures.Add(1)
			_ = peer.Close()
			deliveryErr := &blobDeliveryError{
				kind:     blobOutcomeWriteError,
				attempts: attempt + 1,
				err:      fmt.Errorf("replication: write blob: %w", err),
			}
			log.Error("replication blob delivery", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "outcome", deliveryErr.kind, "attempts", deliveryErr.attempts, "err", deliveryErr.err)
			return deliveryErr
		}
		metrics.BlobsSent.Add(1)
		metrics.BytesSent.Add(uint64(blobBytes))
		log.Info("replicated blob sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "bytes", blobBytes, "attempt", attempt+1, "delivery", "one-shot")

		timer := time.NewTimer(ackTimeout)
		select {
		case <-ack:
			timer.Stop()
			metrics.BlobPutsAccepted.Add(1)
			log.Info("replication blob delivery", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "outcome", blobOutcomeAccepted, "attempts", attempt+1)
			return nil
		case <-ctx.Done():
			timer.Stop()
			if !tracker.remove(peer, key) {
				metrics.BlobPutsAccepted.Add(1)
				return nil
			}
			metrics.BlobPutFailures.Add(1)
			deliveryErr := &blobDeliveryError{kind: blobOutcomeCanceled, attempts: attempt + 1, err: ctx.Err()}
			log.Warn("replication blob delivery", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "outcome", deliveryErr.kind, "attempts", deliveryErr.attempts, "err", deliveryErr.err)
			return deliveryErr
		case <-timer.C:
			if !tracker.remove(peer, key) {
				metrics.BlobPutsAccepted.Add(1)
				return nil
			}
			metrics.BlobAckTimeouts.Add(1)
			if attempt == retries {
				metrics.BlobPutFailures.Add(1)
				deliveryErr := &blobDeliveryError{kind: blobOutcomeAckTimeout, attempts: attempt + 1, err: errBlobAckTimeout}
				log.Error("replication blob delivery", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "outcome", deliveryErr.kind, "attempts", deliveryErr.attempts, "err", deliveryErr.err)
				return deliveryErr
			}
			metrics.BlobRetries.Add(1)
			delay := retryDelayForAttempt(retryDelay, attempt)
			log.Warn("replication blob ack timeout", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "retry", attempt+1, "delay", delay)
			if !sleepContext(ctx, delay) {
				metrics.BlobPutFailures.Add(1)
				deliveryErr := &blobDeliveryError{kind: blobOutcomeCanceled, attempts: attempt + 1, err: ctx.Err()}
				log.Warn("replication blob delivery", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "outcome", deliveryErr.kind, "attempts", deliveryErr.attempts, "err", deliveryErr.err)
				return deliveryErr
			}
		}
	}
	return &blobDeliveryError{kind: blobOutcomeAckTimeout, attempts: retries + 1, err: errBlobAckTimeout}
}

func syncLifecycleRawRecords(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	records []lifecycle.Record,
	limits replication.Limits,
	maxFrameBytes int,
	tracker *putAckTracker,
	ackTimeout time.Duration,
	retries int,
	retryDelay time.Duration,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	if len(records) == 0 {
		return nil
	}
	if store == nil {
		return errors.New("lifecycle: raw blob preflight store unavailable")
	}
	for _, record := range records {
		data, err := store.Get(ctx, record.BlobKey)
		if errors.Is(err, storage.ErrNotFound) {
			return errors.Join(lifecycle.ErrLifecycleBlobMissing, err)
		}
		if err != nil {
			return err
		}
		if err := storage.VerifySHA256Key(record.BlobKey, data); err != nil {
			return err
		}
		payload, err := replication.EncodeBlobPut(record.BlobKey, data, limits)
		if err != nil {
			return err
		}
		if err := sendBlobWithAck(ctx, peer, payload, record.BlobKey, len(data), maxFrameBytes, tracker, ackTimeout, retries, retryDelay, metrics, log); err != nil {
			return err
		}
	}
	return nil
}

func retryDelayForAttempt(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	if base >= maxPutRetryDelay {
		return maxPutRetryDelay
	}
	delay := base
	for i := 0; i < attempt; i++ {
		if delay >= maxPutRetryDelay/2 {
			return maxPutRetryDelay
		}
		delay *= 2
	}
	if delay > maxPutRetryDelay {
		return maxPutRetryDelay
	}
	return delay
}

func putWaitTimeout(ackTimeout, retryDelay time.Duration, retries int) time.Duration {
	total := time.Second
	for range retries + 1 {
		total = addDuration(total, ackTimeout)
	}
	for attempt := 0; attempt < retries; attempt++ {
		total = addDuration(total, retryDelayForAttempt(retryDelay, attempt))
	}
	return total
}

func addDuration(left, right time.Duration) time.Duration {
	if right > 0 && left > maxDuration-right {
		return maxDuration
	}
	return left + right
}

func handleReplicationMessage(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	lister storage.BlobKeyLister,
	msg replication.Message,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
	memoryStore *storage.MemoryStore,
) error {
	switch msg.Type {
	case replication.MessageTypeBlobPut:
		if err := verifyContentKeyIfSHA256(msg.Key, msg.Data); err != nil {
			return err
		}
		existing, err := store.Get(ctx, msg.Key)
		if errors.Is(err, storage.ErrSHA256Mismatch) {
			metrics.CorruptBlobsDetected.Add(1)
			log.Warn("replicated blob corruption detected", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "delivery", "anti-entropy")
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && bytes.Equal(existing, msg.Data) {
			metrics.DuplicateBlobs.Add(1)
			metrics.DuplicateBytes.Add(uint64(len(msg.Data)))
			log.Info("replicated blob duplicate", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "bytes", len(msg.Data))
			return sendBlobAck(peer, msg.Key, limits, maxFrameBytes, metrics, log)
		}
		if err := store.Put(ctx, msg.Key, msg.Data); err != nil {
			return err
		}
		metrics.BlobsStored.Add(1)
		metrics.BytesStored.Add(uint64(len(msg.Data)))
		attrs := []any{"remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "bytes", len(msg.Data)}
		if memoryStore != nil {
			attrs = append(attrs, "blobs", memoryStore.Len())
		}
		log.Info("replicated blob stored", attrs...)
		return sendBlobAck(peer, msg.Key, limits, maxFrameBytes, metrics, log)
	case replication.MessageTypeBlobHas:
		var missing [][]byte
		var err error
		if store != nil {
			metrics.InventoryKeysProbed.Add(uint64(len(msg.Keys)))
			missing, err = missingKeysFromStore(ctx, store, msg.Keys)
		} else if lister != nil {
			var localKeys [][]byte
			localKeys, err = lister.ListKeys(ctx)
			if err == nil {
				missing = missingKeys(msg.Keys, localKeys)
			}
		} else {
			return nil
		}
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		payload, err := replication.EncodeBlobMissing(missing, limits)
		if err != nil {
			return err
		}
		if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
			metrics.SendErrors.Add(1)
			return err
		}
		metrics.MissingKeysRequested.Add(uint64(len(missing)))
		log.Info("replication missing sent", "remote", peer.RemoteAddr().String(), "keys", len(missing))
		return nil
	case replication.MessageTypeBlobMissing:
		deferred, err := sendRequestedBlobsResult(ctx, peer, store, msg.Keys, limits, maxFrameBytes, metrics, log, true)
		if err == nil && len(deferred) > 0 && metrics.repairScheduler != nil {
			metrics.repairScheduler.Schedule(peer, deferred)
		}
		return err
	case replication.MessageTypeBlobGet:
		return sendRequestedBlobs(ctx, peer, store, [][]byte{msg.Key}, limits, maxFrameBytes, metrics, log, false)
	case replication.MessageTypeBlobAck:
		metrics.BlobAcksReceived.Add(1)
		matched := false
		if metrics.ackTracker != nil {
			matched = metrics.ackTracker.ack(peer, msg.Key)
		}
		log.Info("replication ack received", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(msg.Key), "matched", matched)
		return nil
	default:
		return replication.ErrUnknownMessageType
	}
}

func sendBlobAck(
	peer p2p.Peer,
	key []byte,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
) error {
	payload, err := replication.EncodeBlobAck(key, limits)
	if err != nil {
		metrics.SendErrors.Add(1)
		return err
	}
	if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
		metrics.SendErrors.Add(1)
		return err
	}
	metrics.BlobAcksSent.Add(1)
	log.Info("replication ack sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key))
	return nil
}

func sendBlobHas(ctx context.Context, peer p2p.Peer, lister storage.BlobKeyLister, limits replication.Limits, maxFrameBytes int, metrics *replicationMetrics) error {
	if pager, ok := lister.(storage.BlobKeyPager); ok {
		return sendPagedBlobHas(ctx, peer, pager, limits, maxFrameBytes, metrics)
	}
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return err
	}
	return sendBlobHasKeys(ctx, peer, keys, limits, maxFrameBytes, metrics)
}

func sendPagedBlobHas(ctx context.Context, peer p2p.Peer, pager storage.BlobKeyPager, limits replication.Limits, maxFrameBytes int, metrics *replicationMetrics) error {
	pageSize := inventoryBatchSize(limits)
	var cursor []byte
	for {
		keys, next, err := pager.ListKeyPage(ctx, cursor, pageSize)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		if len(next) == 0 || !bytes.Equal(next, keys[len(keys)-1]) {
			return errors.New("replication: blob key page cursor must be last key")
		}
		if len(cursor) > 0 && bytes.Compare(next, cursor) <= 0 {
			return errors.New("replication: blob key page cursor did not advance")
		}
		if err := sendBlobHasKeys(ctx, peer, keys, limits, maxFrameBytes, metrics); err != nil {
			return err
		}
		cursor = append(cursor[:0], next...)
	}
}

func inventoryBatchSize(limits replication.Limits) int {
	if limits.MaxKeys > 0 {
		return limits.MaxKeys
	}
	return replication.DefaultMaxKeys
}

func sendBlobHasKeys(ctx context.Context, peer p2p.Peer, keys [][]byte, limits replication.Limits, maxFrameBytes int, metrics *replicationMetrics) error {
	if len(keys) == 0 {
		return nil
	}
	batchSize := inventoryBatchSize(limits)
	maxPayload := maxFrameBytes
	if maxPayload <= 0 {
		maxPayload = p2p.DefaultMaxFrameBytes
	}
	for start := 0; start < len(keys); {
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			payload, err := replication.EncodeBlobHas(keys[start:end], limits)
			if err != nil {
				return err
			}
			if len(payload) > maxPayload {
				if end-start == 1 {
					return p2p.ErrFrameTooLarge
				}
				end = start + (end-start)/2
				continue
			}
			if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
				return err
			}
			if metrics != nil {
				metrics.InventoryAdvertisements.Add(1)
				metrics.InventoryBytesSent.Add(uint64(len(payload)))
				metrics.InventoryKeysSent.Add(uint64(end - start))
			}
			break
		}
		start = end
	}
	return nil
}

func startPeriodicBlobHas(
	ctx context.Context,
	tr *p2p.TCPTransport,
	scheduler *inventoryExchangeScheduler,
	interval time.Duration,
) {
	if scheduler == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for _, peer := range tr.Peers() {
				scheduler.Start(peer)
			}
		}
	}()
}

// repairIOLimiter bounds one anti-entropy blob read and write across all peers.
// Each peer still owns its own continuation queue, so a blocked peer can hold at
// most one operation at a time.
type repairIOLimiter struct {
	slots   chan struct{}
	metrics *replicationMetrics
}

func newRepairIOLimiter(maxOps int, metrics *replicationMetrics) *repairIOLimiter {
	if maxOps <= 0 {
		maxOps = defaultMaxRepairOps
	}
	slots := make(chan struct{}, maxOps)
	for range maxOps {
		slots <- struct{}{}
	}
	return &repairIOLimiter{slots: slots, metrics: metrics}
}

func (b *repairIOLimiter) acquire(ctx context.Context) (func(), error) {
	if b == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		b.recordRejected()
		return nil, err
	}
	if b.metrics != nil {
		b.metrics.RepairIOOpsQueued.Add(1)
	}
	select {
	case <-b.slots:
	case <-ctx.Done():
		b.finishWaiting()
		b.recordRejected()
		return nil, ctx.Err()
	default:
		if b.metrics != nil {
			b.metrics.RepairIOOpsWaited.Add(1)
		}
		select {
		case <-b.slots:
		case <-ctx.Done():
			b.finishWaiting()
			b.recordRejected()
			return nil, ctx.Err()
		}
	}

	b.finishWaiting()
	if b.metrics != nil {
		b.metrics.RepairIOOpsStarted.Add(1)
		b.metrics.RepairIOOpsInFlight.Add(1)
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			if b.metrics != nil {
				b.metrics.RepairIOOpsCompleted.Add(1)
				b.metrics.RepairIOOpsInFlight.Add(-1)
			}
			b.slots <- struct{}{}
		})
	}, nil
}

func (b *repairIOLimiter) finishWaiting() {
	if b.metrics != nil {
		b.metrics.RepairIOOpsQueued.Add(-1)
	}
}

func (b *repairIOLimiter) recordRejected() {
	if b.metrics != nil {
		b.metrics.RepairIOOpsRejected.Add(1)
	}
}

func sendRequestedBlobs(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	keys [][]byte,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
	repair bool,
) error {
	_, err := sendRequestedBlobsResult(ctx, peer, store, keys, limits, maxFrameBytes, metrics, log, repair)
	return err
}

func sendRequestedBlobsResult(
	ctx context.Context,
	peer p2p.Peer,
	store storage.BlobStore,
	keys [][]byte,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
	repair bool,
) ([][]byte, error) {
	repairBudget := replication.DefaultMaxRepairBytes
	if limits.MaxRepairBytes > 0 {
		repairBudget = limits.MaxRepairBytes
	}
	var repairBytes int
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var deferredKeys [][]byte
		err := func() error {
			if repair && metrics != nil && metrics.repairBudget != nil {
				release, acquireErr := metrics.repairBudget.acquire(ctx)
				if acquireErr != nil {
					return acquireErr
				}
				defer release()
			}

			data, err := store.Get(ctx, key)
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				metrics.SendErrors.Add(1)
				metrics.BlobsSkipped.Add(1)
				log.Warn("replicated blob skipped", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "err", err)
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if repair && repairBytes > 0 && repairBytes+len(data) > repairBudget {
				deferred := len(keys) - i
				metrics.RepairBlobsDeferred.Add(uint64(deferred))
				deferredKeys = cloneBlobKeys(keys[i:])
				log.Warn("replication repair deferred", "remote", peer.RemoteAddr().String(), "keys", deferred, "bytes_sent", repairBytes, "budget_bytes", repairBudget, "delivery", "anti-entropy")
				return nil
			}
			payload, err := replication.EncodeBlobPut(key, data, limits)
			if err != nil {
				metrics.SendErrors.Add(1)
				metrics.BlobsSkipped.Add(1)
				log.Warn("replicated blob skipped", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "err", err)
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
				metrics.SendErrors.Add(1)
				return err
			}
			metrics.BlobsSent.Add(1)
			metrics.BytesSent.Add(uint64(len(data)))
			if repair {
				repairBytes += len(data)
				metrics.RepairBlobsSent.Add(1)
				log.Info("replication repair blob sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "bytes", len(data), "delivery", "anti-entropy")
			} else {
				log.Info("replicated blob sent", "remote", peer.RemoteAddr().String(), "key", formatBlobKey(key), "bytes", len(data), "delivery", "request")
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
		if len(deferredKeys) > 0 {
			return deferredKeys, nil
		}
	}
	return nil, nil
}

type repairContinuationScheduler struct {
	ctx           context.Context
	store         storage.BlobStore
	limits        replication.Limits
	maxKeys       int
	maxFrameBytes int
	metrics       *replicationMetrics
	log           *slog.Logger
	delay         time.Duration

	mu      sync.Mutex
	entries map[string]*repairContinuationEntry
}

type repairContinuationEntry struct {
	peer      p2p.Peer
	pending   map[string][]byte
	inFlight  int
	attempt   int
	running   bool
	scheduled bool
}

func newRepairContinuationScheduler(
	ctx context.Context,
	store storage.BlobStore,
	limits replication.Limits,
	maxFrameBytes int,
	metrics *replicationMetrics,
	log *slog.Logger,
	delay time.Duration,
) *repairContinuationScheduler {
	if delay <= 0 {
		delay = repairContinuationDelay
	}
	maxKeys := limits.MaxKeys
	if maxKeys <= 0 {
		maxKeys = replication.DefaultMaxKeys
	}
	return &repairContinuationScheduler{
		ctx:           ctx,
		store:         store,
		limits:        limits,
		maxKeys:       maxKeys,
		maxFrameBytes: maxFrameBytes,
		metrics:       metrics,
		log:           log,
		delay:         delay,
		entries:       make(map[string]*repairContinuationEntry),
	}
}

func (s *repairContinuationScheduler) Schedule(peer p2p.Peer, keys [][]byte) {
	if s == nil || len(keys) == 0 || s.ctx.Err() != nil {
		return
	}
	id := repairPeerKey(peer)
	launch := false
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		entry = &repairContinuationEntry{
			peer:    peer,
			pending: make(map[string][]byte),
		}
		s.entries[id] = entry
	}
	entry.peer = peer
	dropped := mergeRepairContinuationKeys(entry.pending, keys, s.maxKeys)
	if dropped && s.metrics != nil {
		s.metrics.RepairContinuationsDropped.Add(1)
	}
	if entry.running {
		entry.attempt = 0
	} else if !entry.scheduled {
		entry.attempt = 0
		entry.scheduled = true
		launch = true
	}
	s.updateGaugesLocked()
	s.mu.Unlock()

	if launch {
		if s.metrics != nil {
			s.metrics.RepairContinuationsScheduled.Add(1)
		}
		s.log.Info("replication repair continuation scheduled", "remote", peer.RemoteAddr().String(), "keys", len(keys), "delivery", "anti-entropy")
		go s.wait(id)
	}
}

func (s *repairContinuationScheduler) Forget(peer p2p.Peer) {
	if s == nil {
		return
	}
	s.forgetID(repairPeerKey(peer))
}

func (s *repairContinuationScheduler) wait(id string) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		s.forgetID(id)
	case <-timer.C:
		s.run(id)
	}
}

func (s *repairContinuationScheduler) run(id string) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		s.mu.Unlock()
		return
	}
	entry.scheduled = false
	entry.running = true
	keys := repairContinuationKeys(entry.pending)
	entry.pending = make(map[string][]byte)
	entry.inFlight = len(keys)
	attempt := entry.attempt
	peer := entry.peer
	s.updateGaugesLocked()
	s.mu.Unlock()

	deferred, err := sendRequestedBlobsResult(s.ctx, peer, s.store, keys, s.limits, s.maxFrameBytes, s.metrics, s.log, true)
	if s.metrics != nil {
		s.metrics.RepairContinuationsCompleted.Add(1)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("replication repair continuation failed", "remote", peer.RemoteAddr().String(), "err", err, "delivery", "anti-entropy")
	}

	launch := false
	s.mu.Lock()
	entry = s.entries[id]
	if entry == nil {
		s.mu.Unlock()
		return
	}
	entry.running = false
	entry.inFlight = 0
	if err == nil && len(deferred) > 0 && attempt < maxRepairContinuationAttempts {
		if mergeRepairContinuationKeys(entry.pending, deferred, s.maxKeys) && s.metrics != nil {
			s.metrics.RepairContinuationsDropped.Add(1)
		}
		entry.attempt = attempt + 1
	}
	if len(entry.pending) > 0 {
		entry.scheduled = true
		launch = true
	} else {
		delete(s.entries, id)
	}
	s.updateGaugesLocked()
	s.mu.Unlock()

	if launch {
		if s.metrics != nil {
			s.metrics.RepairContinuationsScheduled.Add(1)
		}
		go s.wait(id)
	}
}

func (s *repairContinuationScheduler) forgetID(id string) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	dropped := ok && (entry.scheduled || entry.running || len(entry.pending) > 0 || entry.inFlight > 0)
	if ok {
		delete(s.entries, id)
	}
	s.updateGaugesLocked()
	s.mu.Unlock()
	if dropped && s.metrics != nil {
		s.metrics.RepairContinuationsDropped.Add(1)
	}
}

// updateGaugesLocked publishes scheduler state as aggregate gauges. The caller must hold s.mu.
func (s *repairContinuationScheduler) updateGaugesLocked() {
	if s == nil || s.metrics == nil {
		return
	}
	var active, pending int64
	for _, entry := range s.entries {
		if entry.scheduled || entry.running {
			active++
		}
		pending += int64(len(entry.pending) + entry.inFlight)
	}
	s.metrics.RepairContinuationsActive.Store(active)
	s.metrics.RepairContinuationKeysPending.Store(pending)
}

func mergeRepairContinuationKeys(dst map[string][]byte, keys [][]byte, maxKeys int) bool {
	if maxKeys <= 0 {
		maxKeys = replication.DefaultMaxKeys
	}
	dropped := false
	for _, key := range keys {
		if _, ok := dst[string(key)]; ok {
			continue
		}
		if len(dst) >= maxKeys {
			dropped = true
			continue
		}
		dst[string(key)] = append([]byte(nil), key...)
	}
	return dropped
}

func repairContinuationKeys(pending map[string][]byte) [][]byte {
	keys := make([][]byte, 0, len(pending))
	for _, key := range pending {
		keys = append(keys, append([]byte(nil), key...))
	}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})
	return keys
}

func repairPeerKey(peer p2p.Peer) string {
	local := ""
	if localPeer, ok := peer.(interface{ LocalAddr() net.Addr }); ok {
		if addr := localPeer.LocalAddr(); addr != nil {
			local = addr.String()
		}
	}
	direction := "inbound"
	if peer.IsOutbound() {
		direction = "outbound"
	}
	return local + "\x00" + peer.RemoteAddr().String() + "\x00" + direction
}

func resolvePutKey(key string, data []byte, contentKey bool) ([]byte, string) {
	if contentKey {
		digest := storage.SHA256Key(data)
		return digest, storage.SHA256KeyHex(data)
	}
	return []byte(key), key
}

func formatBlobKey(key []byte) string {
	if err := storage.ValidateSHA256Key(key); err == nil {
		return hex.EncodeToString(key)
	}
	return string(key)
}

func verifyContentKeyIfSHA256(key, data []byte) error {
	if err := storage.ValidateSHA256Key(key); err != nil {
		return nil
	}
	return storage.VerifySHA256Key(key, data)
}

type frameWriter interface {
	WriteFrame(payload []byte, maxPayload int) error
}

func writePeerFrame(peer p2p.Peer, payload []byte, maxFrameBytes int) error {
	writer, ok := peer.(frameWriter)
	if !ok {
		return errors.New("peer cannot write frames")
	}
	return writer.WriteFrame(payload, maxFrameBytes)
}

func missingKeys(remoteKeys, localKeys [][]byte) [][]byte {
	local := make(map[string]struct{}, len(localKeys))
	for _, key := range localKeys {
		local[string(key)] = struct{}{}
	}
	missing := make([][]byte, 0)
	for _, key := range remoteKeys {
		if _, ok := local[string(key)]; ok {
			continue
		}
		missing = append(missing, append([]byte(nil), key...))
	}
	return missing
}

func cloneBlobKeys(keys [][]byte) [][]byte {
	if keys == nil {
		return nil
	}
	cloned := make([][]byte, len(keys))
	for i, key := range keys {
		cloned[i] = append([]byte(nil), key...)
	}
	return cloned
}

func missingKeysFromStore(ctx context.Context, store storage.BlobStore, remoteKeys [][]byte) ([][]byte, error) {
	missing := make([][]byte, 0, len(remoteKeys))
	for _, key := range remoteKeys {
		has, err := store.Has(ctx, key)
		if err != nil {
			return nil, err
		}
		if !has {
			missing = append(missing, append([]byte(nil), key...))
		}
	}
	return missing, nil
}

type peerReconnector struct {
	ctx    context.Context
	tr     *p2p.TCPTransport
	min    time.Duration
	max    time.Duration
	log    *slog.Logger
	target map[string]struct{}

	mu      sync.Mutex
	dialing map[string]bool
}

func newPeerReconnector(ctx context.Context, tr *p2p.TCPTransport, targets []string, minBackoff, maxBackoff time.Duration, log *slog.Logger) *peerReconnector {
	targetMap := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetMap[target] = struct{}{}
	}
	return &peerReconnector{
		ctx:     ctx,
		tr:      tr,
		min:     minBackoff,
		max:     maxBackoff,
		log:     log,
		target:  targetMap,
		dialing: make(map[string]bool, len(targets)),
	}
}

func (r *peerReconnector) Start() {
	for target := range r.target {
		r.schedule(target, 0)
	}
}

func (r *peerReconnector) OnPeerDisconnected(peer p2p.Peer) {
	if !peer.IsOutbound() {
		return
	}
	target := peer.RemoteAddr().String()
	if _, ok := r.target[target]; !ok {
		return
	}
	r.schedule(target, r.min)
}

func (r *peerReconnector) schedule(target string, initialDelay time.Duration) {
	if r.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	if r.dialing[target] {
		r.mu.Unlock()
		return
	}
	r.dialing[target] = true
	r.mu.Unlock()

	go r.loop(target, initialDelay)
}

func (r *peerReconnector) loop(target string, initialDelay time.Duration) {
	defer func() {
		r.mu.Lock()
		delete(r.dialing, target)
		r.mu.Unlock()
	}()

	delay := r.min
	if initialDelay > 0 {
		if !sleepContext(r.ctx, initialDelay) {
			return
		}
	}

	for {
		if err := r.ctx.Err(); err != nil {
			return
		}
		err := r.tr.Dial(r.ctx, target)
		if err == nil {
			r.log.Info("peer reconnect established", "target", target)
			return
		}
		r.log.Warn("peer reconnect failed", "target", target, "err", err, "next", delay)
		if !sleepContext(r.ctx, delay) {
			return
		}
		delay = nextBackoff(delay, r.max)
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maxDelay time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > maxDelay {
		return maxDelay
	}
	return next
}

func parsePeerTargets(dial, peers string) ([]string, error) {
	peerList, err := parsePeerList(peers)
	if err != nil {
		return nil, err
	}
	return combinePeerTargets(strings.TrimSpace(dial), peerList), nil
}

func parsePeerList(peers string) ([]string, error) {
	if strings.TrimSpace(peers) == "" {
		return nil, nil
	}
	targets := make([]string, 0, 1)
	for _, part := range strings.Split(peers, ",") {
		target := strings.TrimSpace(part)
		if target == "" {
			return nil, fmt.Errorf("peers: empty peer address")
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func loadTLSCAPool(path, flagName string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tls: read %s: %w", strings.TrimPrefix(flagName, "-tls-"), err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("tls: no certificates parsed from %s", flagName)
	}
	return pool, nil
}

type tlsCredentialInput struct {
	flagName    string
	certificate *tls.Certificate
}

type tlsCredentialValidity struct {
	notBefore time.Time
	notAfter  time.Time
}

type tlsCredentialHealth struct {
	credentials   []tlsCredentialValidity
	warningWindow time.Duration
}

func newTLSCredentialHealth(now time.Time, warningWindow time.Duration, inputs ...tlsCredentialInput) (*tlsCredentialHealth, error) {
	health := &tlsCredentialHealth{warningWindow: warningWindow}
	for _, input := range inputs {
		if input.certificate == nil {
			continue
		}
		if len(input.certificate.Certificate) == 0 {
			return nil, fmt.Errorf("tls: %s certificate has no leaf", input.flagName)
		}
		leaf, err := x509.ParseCertificate(input.certificate.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("tls: parse %s certificate: %w", input.flagName, err)
		}
		if now.Before(leaf.NotBefore) {
			return nil, fmt.Errorf("tls: %s certificate is not valid before %s", input.flagName, leaf.NotBefore.UTC().Format(time.RFC3339))
		}
		if !now.Before(leaf.NotAfter) {
			return nil, fmt.Errorf("tls: %s certificate expired at %s", input.flagName, leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		health.credentials = append(health.credentials, tlsCredentialValidity{
			notBefore: leaf.NotBefore,
			notAfter:  leaf.NotAfter,
		})
	}
	return health, nil
}

func (h *tlsCredentialHealth) Snapshot(now time.Time) map[string]int64 {
	if h == nil {
		return map[string]int64{}
	}
	var earliestExpiry time.Time
	var expired, notYetValid, expiringSoon int64
	warningDeadline := now.Add(h.warningWindow)
	for _, credential := range h.credentials {
		if earliestExpiry.IsZero() || credential.notAfter.Before(earliestExpiry) {
			earliestExpiry = credential.notAfter
		}
		switch {
		case now.Before(credential.notBefore):
			notYetValid++
		case !now.Before(credential.notAfter):
			expired++
		case h.warningWindow > 0 && !credential.notAfter.After(warningDeadline):
			expiringSoon++
		}
	}
	var expiryTimestamp int64
	if !earliestExpiry.IsZero() {
		expiryTimestamp = earliestExpiry.Unix()
	}
	return map[string]int64{
		"tls_certificates_configured":              int64(len(h.credentials)),
		"tls_certificate_expiry_timestamp_seconds": expiryTimestamp,
		"tls_certificates_expired":                 expired,
		"tls_certificates_not_yet_valid":           notYetValid,
		"tls_certificates_expiring_soon":           expiringSoon,
	}
}

func (h *tlsCredentialHealth) Ready(now time.Time) bool {
	if h == nil {
		return true
	}
	for _, credential := range h.credentials {
		if now.Before(credential.notBefore) || !now.Before(credential.notAfter) {
			return false
		}
	}
	return true
}

func parsePeerIdentityList(identities string) ([]string, error) {
	if strings.TrimSpace(identities) == "" {
		return nil, nil
	}
	allowed := make([]string, 0, 1)
	for _, part := range strings.Split(identities, ",") {
		identity := strings.TrimSpace(part)
		if identity == "" {
			return nil, fmt.Errorf("peers: empty peer identity")
		}
		allowed = append(allowed, identity)
	}
	return allowed, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	if fs == nil {
		return false
	}
	set := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			set = true
		}
	})
	return set
}

func combinePeerTargets(dial string, peers []string) []string {
	targets := make([]string, 0, len(peers)+1)
	if dial != "" {
		targets = append(targets, dial)
	}
	targets = append(targets, peers...)
	return targets
}

func validateReconnectBackoff(minBackoff, maxBackoff time.Duration) error {
	if minBackoff <= 0 {
		return fmt.Errorf("peers: -peer-reconnect-min must be greater than zero")
	}
	if maxBackoff < minBackoff {
		return fmt.Errorf("peers: -peer-reconnect-max must be greater than or equal to -peer-reconnect-min")
	}
	return nil
}

type replicationMetrics struct {
	BlobsStored                    atomic.Uint64
	BytesStored                    atomic.Uint64
	DuplicateBlobs                 atomic.Uint64
	DuplicateBytes                 atomic.Uint64
	ApplyErrors                    atomic.Uint64
	BlobsSent                      atomic.Uint64
	BytesSent                      atomic.Uint64
	BlobsSkipped                   atomic.Uint64
	BlobAcksSent                   atomic.Uint64
	BlobAcksReceived               atomic.Uint64
	BlobAcksMatched                atomic.Uint64
	BlobAcksPending                atomic.Int64
	BlobAckTimeouts                atomic.Uint64
	BlobRetries                    atomic.Uint64
	BlobPutsAccepted               atomic.Uint64
	BlobPutFailures                atomic.Uint64
	BlobWriteErrors                atomic.Uint64
	SendErrors                     atomic.Uint64
	InventoryStatusScansStarted    atomic.Uint64
	InventoryStatusScansCompleted  atomic.Uint64
	InventoryStatusScansFailed     atomic.Uint64
	InventoryStatusKeysScanned     atomic.Uint64
	InventoryStatusKeyBytesScanned atomic.Uint64
	InventoryStatusScanDurationMS  atomic.Uint64
	InventoryAdvertisements        atomic.Uint64
	InventoryBytesSent             atomic.Uint64
	InventoryKeysSent              atomic.Uint64
	InventoryKeysProbed            atomic.Uint64
	InventoryExchangesStarted      atomic.Uint64
	InventoryExchangesCompleted    atomic.Uint64
	InventoryExchangesLimited      atomic.Uint64
	InventoryExchangesDropped      atomic.Uint64
	InventoryExchangesActive       atomic.Int64
	MissingKeysRequested           atomic.Uint64
	RepairBlobsSent                atomic.Uint64
	RepairBlobsDeferred            atomic.Uint64
	CorruptBlobsDetected           atomic.Uint64
	RepairContinuationsScheduled   atomic.Uint64
	RepairContinuationsCompleted   atomic.Uint64
	RepairContinuationsDropped     atomic.Uint64
	RepairContinuationsActive      atomic.Int64
	RepairContinuationKeysPending  atomic.Int64
	RepairIOOpsStarted             atomic.Uint64
	RepairIOOpsCompleted           atomic.Uint64
	RepairIOOpsWaited              atomic.Uint64
	RepairIOOpsRejected            atomic.Uint64
	RepairIOOpsInFlight            atomic.Int64
	RepairIOOpsQueued              atomic.Int64
	ackTracker                     *putAckTracker
	repairBudget                   *repairIOLimiter
	repairScheduler                *repairContinuationScheduler
}

func (m *replicationMetrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"replication_blobs_stored":                       int64(m.BlobsStored.Load()),
		"replication_bytes_stored":                       int64(m.BytesStored.Load()),
		"replication_duplicate_blobs":                    int64(m.DuplicateBlobs.Load()),
		"replication_duplicate_bytes":                    int64(m.DuplicateBytes.Load()),
		"replication_apply_errors":                       int64(m.ApplyErrors.Load()),
		"replication_blobs_sent":                         int64(m.BlobsSent.Load()),
		"replication_bytes_sent":                         int64(m.BytesSent.Load()),
		"replication_blobs_skipped":                      int64(m.BlobsSkipped.Load()),
		"replication_blob_acks_sent":                     int64(m.BlobAcksSent.Load()),
		"replication_blob_acks_received":                 int64(m.BlobAcksReceived.Load()),
		"replication_blob_acks_matched":                  int64(m.BlobAcksMatched.Load()),
		"replication_blob_acks_pending":                  m.BlobAcksPending.Load(),
		"replication_blob_ack_timeouts":                  int64(m.BlobAckTimeouts.Load()),
		"replication_blob_retries":                       int64(m.BlobRetries.Load()),
		"replication_blob_puts_accepted":                 int64(m.BlobPutsAccepted.Load()),
		"replication_blob_put_failures":                  int64(m.BlobPutFailures.Load()),
		"replication_blob_write_errors":                  int64(m.BlobWriteErrors.Load()),
		"replication_send_errors":                        int64(m.SendErrors.Load()),
		"replication_inventory_status_scans_started":     int64(m.InventoryStatusScansStarted.Load()),
		"replication_inventory_status_scans_completed":   int64(m.InventoryStatusScansCompleted.Load()),
		"replication_inventory_status_scans_failed":      int64(m.InventoryStatusScansFailed.Load()),
		"replication_inventory_status_keys_scanned":      int64(m.InventoryStatusKeysScanned.Load()),
		"replication_inventory_status_key_bytes_scanned": int64(m.InventoryStatusKeyBytesScanned.Load()),
		"replication_inventory_status_scan_duration_ms":  int64(m.InventoryStatusScanDurationMS.Load()),
		"replication_inventory_advertisements":           int64(m.InventoryAdvertisements.Load()),
		"replication_inventory_bytes_sent":               int64(m.InventoryBytesSent.Load()),
		"replication_inventory_keys_sent":                int64(m.InventoryKeysSent.Load()),
		"replication_inventory_keys_probed":              int64(m.InventoryKeysProbed.Load()),
		"replication_inventory_exchanges_started":        int64(m.InventoryExchangesStarted.Load()),
		"replication_inventory_exchanges_completed":      int64(m.InventoryExchangesCompleted.Load()),
		"replication_inventory_exchanges_limited":        int64(m.InventoryExchangesLimited.Load()),
		"replication_inventory_exchanges_dropped":        int64(m.InventoryExchangesDropped.Load()),
		"replication_inventory_exchanges_active":         m.InventoryExchangesActive.Load(),
		"replication_missing_keys_requested":             int64(m.MissingKeysRequested.Load()),
		"replication_repair_blobs_sent":                  int64(m.RepairBlobsSent.Load()),
		"replication_repair_blobs_deferred":              int64(m.RepairBlobsDeferred.Load()),
		"replication_corrupt_blobs_detected":             int64(m.CorruptBlobsDetected.Load()),
		"replication_repair_continuations_scheduled":     int64(m.RepairContinuationsScheduled.Load()),
		"replication_repair_continuations_completed":     int64(m.RepairContinuationsCompleted.Load()),
		"replication_repair_continuations_dropped":       int64(m.RepairContinuationsDropped.Load()),
		"replication_repair_continuations_active":        m.RepairContinuationsActive.Load(),
		"replication_repair_continuation_keys_pending":   m.RepairContinuationKeysPending.Load(),
		"replication_repair_io_ops_started":              int64(m.RepairIOOpsStarted.Load()),
		"replication_repair_io_ops_completed":            int64(m.RepairIOOpsCompleted.Load()),
		"replication_repair_io_ops_waited":               int64(m.RepairIOOpsWaited.Load()),
		"replication_repair_io_ops_rejected":             int64(m.RepairIOOpsRejected.Load()),
		"replication_repair_io_ops_in_flight":            m.RepairIOOpsInFlight.Load(),
		"replication_repair_io_ops_queued":               m.RepairIOOpsQueued.Load(),
	}
}

type peerStatus struct {
	RemoteAddr     string   `json:"remote_addr"`
	LocalAddr      string   `json:"local_addr,omitempty"`
	Outbound       bool     `json:"outbound"`
	ConnectedAt    string   `json:"connected_at,omitempty"`
	ConnectedForMS int64    `json:"connected_for_ms"`
	AuthMethod     string   `json:"auth_method"`
	AuthIdentity   string   `json:"auth_identity,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
}

type peersResponse struct {
	ActivePeers int          `json:"active_peers"`
	Peers       []peerStatus `json:"peers"`
}

func snapshotPeers(peers []p2p.PeerSnapshot, now time.Time) peersResponse {
	statuses := make([]peerStatus, 0, len(peers))
	for _, peer := range peers {
		connectedAt := ""
		var connectedForMS int64
		if !peer.ConnectedAt.IsZero() {
			connectedAt = peer.ConnectedAt.UTC().Format(time.RFC3339Nano)
			if now.After(peer.ConnectedAt) {
				connectedForMS = now.Sub(peer.ConnectedAt).Milliseconds()
			}
		}
		statuses = append(statuses, peerStatus{
			RemoteAddr:     peer.RemoteAddr,
			LocalAddr:      peer.LocalAddr,
			Outbound:       peer.Outbound,
			ConnectedAt:    connectedAt,
			ConnectedForMS: connectedForMS,
			AuthMethod:     peer.AuthMethod,
			AuthIdentity:   peer.AuthIdentity,
			Capabilities:   append([]string(nil), peer.Capabilities...),
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].RemoteAddr == statuses[j].RemoteAddr {
			return !statuses[i].Outbound && statuses[j].Outbound
		}
		return statuses[i].RemoteAddr < statuses[j].RemoteAddr
	})
	return peersResponse{
		ActivePeers: len(statuses),
		Peers:       statuses,
	}
}

type inventoryStatusResponse struct {
	Enabled         bool   `json:"enabled"`
	Ready           bool   `json:"ready"`
	ScanConsistency string `json:"scan_consistency"`
	Keys            int    `json:"keys"`
	KeyBytes        int64  `json:"key_bytes"`
	Digest          string `json:"digest,omitempty"`
}

func snapshotInventoryStatus(ctx context.Context, lister storage.BlobKeyLister) (inventoryStatusResponse, error) {
	status := inventoryStatusResponse{
		Ready:           true,
		ScanConsistency: "live",
	}
	if lister == nil {
		return status, nil
	}
	summary, err := storage.SummarizeInventory(ctx, lister)
	if err != nil {
		return inventoryStatusResponse{}, err
	}
	status.Enabled = true
	status.Keys = summary.KeyCount
	status.KeyBytes = summary.KeyBytes
	status.Digest = summary.DigestHex()
	return status, nil
}

func observeInventoryStatus(ctx context.Context, lister storage.BlobKeyLister, metrics *replicationMetrics) (inventoryStatusResponse, error) {
	if lister == nil || metrics == nil {
		return snapshotInventoryStatus(ctx, lister)
	}
	startedAt := time.Now()
	metrics.InventoryStatusScansStarted.Add(1)
	status, err := snapshotInventoryStatus(ctx, lister)
	metrics.InventoryStatusScanDurationMS.Add(uint64(time.Since(startedAt) / time.Millisecond))
	if err != nil {
		metrics.InventoryStatusScansFailed.Add(1)
		return inventoryStatusResponse{}, err
	}
	metrics.InventoryStatusScansCompleted.Add(1)
	metrics.InventoryStatusKeysScanned.Add(uint64(status.Keys))
	metrics.InventoryStatusKeyBytesScanned.Add(uint64(status.KeyBytes))
	return status, nil
}

func startHealth(addr string, tr *p2p.TCPTransport, replMetrics *replicationMetrics, keyLister storage.BlobKeyLister, tlsHealth *tlsCredentialHealth, lifecycleState *lifecycleRuntime, log *slog.Logger) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !tr.Ready() || !tlsHealth.Ready(time.Now().UTC()) || !lifecycleState.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snapshot := tr.Metrics().Snapshot()
		for key, value := range replMetrics.Snapshot() {
			snapshot[key] = value
		}
		for key, value := range tlsHealth.Snapshot(time.Now().UTC()) {
			snapshot[key] = value
		}
		for key, value := range lifecycleState.Metrics() {
			snapshot[key] = value
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snapshot)
	})
	mux.HandleFunc("/lifecycle/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(lifecycleState.Status())
	})
	mux.HandleFunc("/inventory/status", func(w http.ResponseWriter, req *http.Request) {
		status, err := observeInventoryStatus(req.Context(), keyLister, replMetrics)
		if err != nil {
			http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(status)
	})
	mux.HandleFunc("/peers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snapshotPeers(tr.PeerSnapshots(), time.Now()))
	})
	mux.HandleFunc("/metrics/prometheus", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		snapshot := tr.Metrics().Snapshot()
		for key, value := range replMetrics.Snapshot() {
			snapshot[key] = value
		}
		for key, value := range tlsHealth.Snapshot(time.Now().UTC()) {
			snapshot[key] = value
		}
		for key, value := range lifecycleState.Metrics() {
			snapshot[key] = value
		}
		writePrometheusMetrics(w, snapshot)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: defaultHealthReadHeaderTimeout,
		ReadTimeout:       defaultHealthReadTimeout,
		WriteTimeout:      defaultHealthWriteTimeout,
		IdleTimeout:       defaultHealthIdleTimeout,
		MaxHeaderBytes:    defaultHealthMaxHeaderBytes,
	}
	go func() {
		log.Info("health", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
		}
	}()
	return srv, nil
}

func writePrometheusMetrics(w io.Writer, snapshot map[string]int64) {
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		metricName := "streamhive_" + key
		_, _ = fmt.Fprintf(w, "# HELP %s StreamHive %s.\n# TYPE %s %s\n%s %d\n", metricName, strings.ReplaceAll(key, "_", " "), metricName, prometheusMetricType(key), metricName, snapshot[key])
	}
}

var prometheusGaugeMetrics = map[string]struct{}{
	"active_peers":                                 {},
	"lifecycle_enabled":                            {},
	"lifecycle_ready":                              {},
	"lifecycle_authority_epoch":                    {},
	"lifecycle_authority_sequence":                 {},
	"lifecycle_journal_floor_epoch":                {},
	"lifecycle_journal_floor_sequence":             {},
	"lifecycle_journal_tail_epoch":                 {},
	"lifecycle_journal_tail_sequence":              {},
	"lifecycle_journal_entries":                    {},
	"lifecycle_journal_bytes":                      {},
	"lifecycle_logical_records":                    {},
	"lifecycle_tombstones":                         {},
	"lifecycle_membership_configured":              {},
	"lifecycle_membership_members":                 {},
	"lifecycle_membership_acknowledged":            {},
	"lifecycle_membership_min_epoch":               {},
	"lifecycle_membership_min_sequence":            {},
	"lifecycle_compaction_target_epoch":            {},
	"lifecycle_compaction_target_sequence":         {},
	"lifecycle_compaction_blocked":                 {},
	"lifecycle_repair_sessions_active":             {},
	"replication_blob_acks_pending":                {},
	"replication_inventory_exchanges_active":       {},
	"replication_repair_continuations_active":      {},
	"replication_repair_continuation_keys_pending": {},
	"replication_repair_io_ops_in_flight":          {},
	"replication_repair_io_ops_queued":             {},
	"shutdown_state":                               {},
	"shutdown_tracked_peers":                       {},
	"shutdown_tracked_goroutines":                  {},
	"tls_certificate_expiry_timestamp_seconds":     {},
	"tls_certificates_configured":                  {},
	"tls_certificates_expired":                     {},
	"tls_certificates_expiring_soon":               {},
	"tls_certificates_not_yet_valid":               {},
}

func prometheusMetricType(key string) string {
	if _, ok := prometheusGaugeMetrics[key]; ok {
		return "gauge"
	}
	return "counter"
}
