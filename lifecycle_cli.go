package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/storage"
)

type lifecycleCLIConfig struct {
	enabled       bool
	dir           string
	repairLimits  lifecycle.RepairLimits
	recordLimits  lifecycle.Limits
	journalLimits lifecycle.JournalOptions
}

type lifecycleRuntime struct {
	checkpointPath string
	repairLimits   lifecycle.RepairLimits
	journal        *lifecycle.Journal
	authority      *lifecycle.Authority
	watermarks     *lifecycle.WatermarkBook
	coordinator    *lifecycle.RepairCoordinator
	state          *lifecycle.Store
	applier        *lifecycle.Applier

	snapshotMu sync.RWMutex
	snapshot   *lifecycle.Checkpoint

	sessionsMu sync.Mutex
	sessions   map[string]lifecycleSessionEntry
	sessionsWG sync.WaitGroup
	metrics    lifecycleRuntimeMetrics
}

type lifecycleSessionEntry struct {
	session *lifecycle.RepairSession
	cancel  context.CancelFunc
}

type lifecycleRuntimeMetrics struct {
	SessionsStarted   atomic.Uint64
	SessionsCompleted atomic.Uint64
	SessionErrors     atomic.Uint64
	SessionsActive    atomic.Int64
	FramesReceived    atomic.Uint64
	FrameErrors       atomic.Uint64
}

func (c lifecycleCLIConfig) validate(blobStore storage.BlobStore, peerAuthToken, peerID string) error {
	if !c.enabled {
		if c.dir != "" {
			return errors.New("lifecycle: -lifecycle-dir requires -lifecycle")
		}
		return nil
	}
	if blobStore == nil {
		return errors.New("lifecycle: -lifecycle requires -replicate")
	}
	if c.dir == "" {
		return errors.New("lifecycle: -lifecycle-dir is required with -lifecycle")
	}
	if peerAuthToken == "" {
		return errors.New("lifecycle: -lifecycle requires -peer-auth-token")
	}
	if peerID == "" {
		return errors.New("lifecycle: -lifecycle requires -peer-id")
	}
	if c.repairLimits.MaxRecords < 0 || c.repairLimits.MaxLogicalKeyBytes < 0 ||
		c.repairLimits.MaxMetadataBytes < 0 || c.repairLimits.MaxFrameBytes < 0 {
		return errors.New("lifecycle: repair limits must be zero or greater")
	}
	return nil
}

func openLifecycleRuntime(ctx context.Context, config lifecycleCLIConfig, blobs storage.BlobStore, peerAuthToken, peerID string) (*lifecycleRuntime, error) {
	if !config.enabled {
		return nil, nil
	}
	if err := config.validate(blobs, peerAuthToken, peerID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.dir, 0o700); err != nil {
		return nil, fmt.Errorf("lifecycle: create state directory: %w", err)
	}

	recordLimits := config.recordLimits
	journal, _, err := lifecycle.OpenJournal(filepath.Join(config.dir, "journal"), lifecycle.JournalOptions{
		Limits:            recordLimits,
		MaxJournalBytes:   config.journalLimits.MaxJournalBytes,
		MaxJournalEntries: config.journalLimits.MaxJournalEntries,
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open journal: %w", err)
	}
	closeJournal := true
	defer func() {
		if closeJournal {
			_ = journal.Close()
		}
	}()
	authority, err := lifecycle.OpenAuthority(ctx, filepath.Join(config.dir, "authority"), peerID, lifecycle.AuthorityOptions{
		Limits:   recordLimits,
		Observed: journal.LastVersion(),
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open authority: %w", err)
	}

	watermarks, err := lifecycle.OpenWatermarkBook(filepath.Join(config.dir, "watermarks"), lifecycle.WatermarkOptions{})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open watermarks: %w", err)
	}
	state := lifecycle.NewStore(recordLimits)
	checkpointPath := filepath.Join(config.dir, "checkpoint")
	checkpoint, err := lifecycle.LoadCheckpoint(ctx, checkpointPath, recordLimits)
	hasCheckpoint := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("lifecycle: load checkpoint: %w", err)
	}
	if hasCheckpoint {
		if checkpoint.Watermark != journal.Floor() {
			return nil, fmt.Errorf("lifecycle: checkpoint watermark %v does not match journal floor %v", checkpoint.Watermark, journal.Floor())
		}
		if err := verifyLifecycleRecords(ctx, blobs, checkpoint.Records); err != nil {
			return nil, fmt.Errorf("lifecycle: verify checkpoint: %w", err)
		}
		if err := state.ReplaceSnapshot(checkpoint.Records); err != nil {
			return nil, fmt.Errorf("lifecycle: restore checkpoint: %w", err)
		}
	} else if !journal.Floor().IsZero() {
		return nil, errors.New("lifecycle: journal floor requires a checkpoint")
	}
	if err := journal.Replay(ctx, func(record lifecycle.Record) error {
		if err := verifyLifecycleRecords(ctx, blobs, []lifecycle.Record{record}); err != nil {
			return err
		}
		_, err := state.Apply(record)
		return err
	}); err != nil {
		return nil, fmt.Errorf("lifecycle: replay journal: %w", err)
	}
	applier, err := lifecycle.NewApplier(blobs, state, journal, recordLimits)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: create applier: %w", err)
	}
	coordinator, err := lifecycle.NewRepairCoordinator(journal, watermarks, config.repairLimits)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: create coordinator: %w", err)
	}
	runtime := &lifecycleRuntime{
		checkpointPath: checkpointPath,
		repairLimits:   config.repairLimits,
		journal:        journal,
		authority:      authority,
		watermarks:     watermarks,
		coordinator:    coordinator,
		state:          state,
		applier:        applier,
		sessions:       make(map[string]lifecycleSessionEntry),
	}
	if hasCheckpoint {
		runtime.snapshot = &checkpoint
	}
	closeJournal = false
	return runtime, nil
}

func (r *lifecycleRuntime) nextVersion(ctx context.Context) (lifecycle.Version, error) {
	if r == nil || r.authority == nil || r.journal == nil {
		return lifecycle.Version{}, errors.New("lifecycle: runtime authority unavailable")
	}
	return r.authority.Next(ctx, r.journal.LastVersion())
}

func verifyLifecycleRecords(ctx context.Context, blobs storage.BlobStore, records []lifecycle.Record) error {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.State != lifecycle.StatePresent {
			continue
		}
		data, err := blobs.Get(ctx, record.BlobKey)
		if errors.Is(err, storage.ErrNotFound) {
			return errors.Join(lifecycle.ErrLifecycleBlobMissing, err)
		}
		if err != nil {
			return err
		}
		if err := storage.VerifySHA256Key(record.BlobKey, data); err != nil {
			return err
		}
	}
	return nil
}

func (r *lifecycleRuntime) Close() error {
	if r == nil || r.journal == nil {
		return nil
	}
	r.sessionsMu.Lock()
	for key, entry := range r.sessions {
		entry.cancel()
		delete(r.sessions, key)
	}
	r.metrics.SessionsActive.Store(0)
	r.sessionsMu.Unlock()
	r.sessionsWG.Wait()
	return r.journal.Close()
}

func (r *lifecycleRuntime) Snapshot() *lifecycle.Checkpoint {
	if r == nil {
		return nil
	}
	r.snapshotMu.RLock()
	defer r.snapshotMu.RUnlock()
	if r.snapshot == nil {
		return nil
	}
	checkpoint := *r.snapshot
	checkpoint.Records = append([]lifecycle.Record(nil), r.snapshot.Records...)
	return &checkpoint
}

func (r *lifecycleRuntime) AttachPeer(ctx context.Context, peer p2p.Peer, maxFrameBytes int, log *slog.Logger) {
	if r == nil || peer == nil || !lifecycle.HasLifecycleCapability(authCapabilitiesForPeer(peer)) {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	framePeer, ok := peer.(lifecycle.RepairFramePeer)
	if !ok {
		r.metrics.SessionErrors.Add(1)
		log.Warn("lifecycle peer does not expose repair frames", "remote", peer.RemoteAddr().String())
		return
	}
	remoteID := authIdentityForPeer(peer)
	if remoteID == "" {
		r.metrics.SessionErrors.Add(1)
		log.Warn("lifecycle peer has no authenticated identity", "remote", peer.RemoteAddr().String())
		return
	}
	session, err := lifecycle.NewRepairSession(lifecycle.RepairSessionOptions{
		Peer:           framePeer,
		Coordinator:    r.coordinator,
		Applier:        r.applier,
		PeerID:         remoteID,
		Snapshot:       r.Snapshot(),
		CheckpointPath: r.checkpointPath,
		MaxFrameBytes:  maxFrameBytes,
	})
	if err != nil {
		r.metrics.SessionErrors.Add(1)
		log.Warn("lifecycle repair session rejected", "remote", peer.RemoteAddr().String(), "err", err)
		return
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	key := repairPeerKey(peer)
	r.sessionsMu.Lock()
	if previous, exists := r.sessions[key]; exists {
		previous.cancel()
		r.metrics.SessionsActive.Add(-1)
	}
	r.sessions[key] = lifecycleSessionEntry{session: session, cancel: cancel}
	r.metrics.SessionsStarted.Add(1)
	r.metrics.SessionsActive.Add(1)
	r.sessionsWG.Add(1)
	r.sessionsMu.Unlock()

	go func() {
		defer r.sessionsWG.Done()
		err := session.Run(sessionCtx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.metrics.SessionErrors.Add(1)
			log.Warn("lifecycle repair session", "remote", peer.RemoteAddr().String(), "err", err)
		} else if err == nil {
			r.metrics.SessionsCompleted.Add(1)
		}
	}()
}

func (r *lifecycleRuntime) ForgetPeer(peer p2p.Peer) {
	if r == nil || peer == nil {
		return
	}
	key := repairPeerKey(peer)
	r.sessionsMu.Lock()
	entry, exists := r.sessions[key]
	if exists {
		delete(r.sessions, key)
		entry.cancel()
		r.metrics.SessionsActive.Add(-1)
	}
	r.sessionsMu.Unlock()
}

func (r *lifecycleRuntime) HandleFrame(ctx context.Context, peer p2p.Peer, payload []byte) error {
	if r == nil || peer == nil {
		return lifecycle.ErrLifecycleCapabilityRequired
	}
	key := repairPeerKey(peer)
	r.sessionsMu.Lock()
	entry, exists := r.sessions[key]
	r.sessionsMu.Unlock()
	if !exists {
		return errors.New("lifecycle: repair session unavailable")
	}
	r.metrics.FramesReceived.Add(1)
	if err := entry.session.Handle(ctx, payload); err != nil {
		r.metrics.FrameErrors.Add(1)
		return err
	}
	return nil
}

func (r *lifecycleRuntime) Ready() bool {
	return r == nil || r.journal != nil
}

func (r *lifecycleRuntime) Metrics() map[string]int64 {
	if r == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"lifecycle_enabled":                   1,
		"lifecycle_repair_sessions_started":   int64(r.metrics.SessionsStarted.Load()),
		"lifecycle_repair_sessions_completed": int64(r.metrics.SessionsCompleted.Load()),
		"lifecycle_repair_sessions_active":    r.metrics.SessionsActive.Load(),
		"lifecycle_repair_session_errors":     int64(r.metrics.SessionErrors.Load()),
		"lifecycle_repair_frames_received":    int64(r.metrics.FramesReceived.Load()),
		"lifecycle_repair_frame_errors":       int64(r.metrics.FrameErrors.Load()),
	}
}

func authCapabilitiesForPeer(peer p2p.Peer) []string {
	if provider, ok := peer.(interface{ AuthCapabilities() []string }); ok {
		return provider.AuthCapabilities()
	}
	return nil
}

func isLifecycleRepairPayload(payload []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	switch envelope.Type {
	case lifecycle.RepairBatchMessageType, lifecycle.RepairSnapshotMessageType, lifecycle.RepairAckMessageType:
		return true
	default:
		return false
	}
}
