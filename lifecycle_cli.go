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
	"time"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/storage"
)

type lifecycleCLIConfig struct {
	enabled              bool
	dir                  string
	membershipConfigured bool
	membershipMembers    []string
	repairLimits         lifecycle.RepairLimits
	recordLimits         lifecycle.Limits
	journalLimits        lifecycle.JournalOptions
}

type lifecycleRawRecordSync func(context.Context, p2p.Peer, []lifecycle.Record) error

type lifecycleRuntime struct {
	checkpointPath string
	repairLimits   lifecycle.RepairLimits
	journal        *lifecycle.Journal
	authority      *lifecycle.Authority
	blobs          storage.BlobStore
	watermarks     *lifecycle.WatermarkBook
	membership     *lifecycle.MembershipBook
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
	active  bool
}

type lifecycleStatus struct {
	Enabled                 bool              `json:"enabled"`
	Ready                   bool              `json:"ready"`
	Readiness               string            `json:"readiness"`
	AuthorityID             string            `json:"authority_id,omitempty"`
	AuthorityVersion        lifecycle.Version `json:"authority_version"`
	JournalFloor            lifecycle.Version `json:"journal_floor"`
	JournalTail             lifecycle.Version `json:"journal_tail"`
	JournalEntries          int               `json:"journal_entries"`
	JournalBytes            int64             `json:"journal_bytes"`
	LogicalRecords          int               `json:"logical_records"`
	Tombstones              int               `json:"tombstones"`
	MembershipConfigured    bool              `json:"membership_configured"`
	MembershipMembers       int               `json:"membership_members"`
	MembershipAcknowledged  int               `json:"membership_acknowledged"`
	MembershipMinimum       lifecycle.Version `json:"membership_minimum"`
	CompactionTarget        lifecycle.Version `json:"compaction_target"`
	CompactionBlocked       bool              `json:"compaction_blocked"`
	CompactionBlockedReason string            `json:"compaction_blocked_reason,omitempty"`
	RepairSessionsActive    int64             `json:"repair_sessions_active"`
	RepairSessionsStarted   uint64            `json:"repair_sessions_started"`
	RepairSessionsCompleted uint64            `json:"repair_sessions_completed"`
}

type lifecycleRuntimeMetrics struct {
	SessionsStarted   atomic.Uint64
	SessionsCompleted atomic.Uint64
	SessionErrors     atomic.Uint64
	SessionsActive    atomic.Int64
	FramesReceived    atomic.Uint64
	FrameErrors       atomic.Uint64
	MutationsStarted  atomic.Uint64
	MutationsApplied  atomic.Uint64
	MutationErrors    atomic.Uint64
}

func (c lifecycleCLIConfig) validate(blobStore storage.BlobStore, peerAuthToken, peerID string) error {
	if !c.enabled {
		if c.dir != "" {
			return errors.New("lifecycle: -lifecycle-dir requires -lifecycle")
		}
		if c.membershipConfigured {
			return errors.New("lifecycle: -lifecycle-members requires -lifecycle")
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
	membership, err := lifecycle.OpenMembershipBook(filepath.Join(config.dir, "membership"), lifecycle.MembershipOptions{})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open membership: %w", err)
	}
	if config.membershipConfigured {
		if err := membership.Replace(ctx, config.membershipMembers); err != nil {
			return nil, fmt.Errorf("lifecycle: configure membership: %w", err)
		}
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
		blobs:          blobs,
		watermarks:     watermarks,
		membership:     membership,
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

func (r *lifecycleRuntime) put(ctx context.Context, namespace, logicalKey string, data, blobKey []byte) (lifecycle.Record, lifecycle.ApplyResult, error) {
	if r == nil || r.applier == nil {
		return lifecycle.Record{}, lifecycle.ApplyResult{}, errors.New("lifecycle: runtime applier unavailable")
	}
	r.metrics.MutationsStarted.Add(1)
	if err := r.state.ValidateKey([]byte(namespace), []byte(logicalKey)); err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	if blobKey == nil {
		blobKey = storage.SHA256Key(data)
	} else {
		blobKey = append([]byte(nil), blobKey...)
		if err := storage.VerifySHA256Key(blobKey, data); err != nil {
			r.metrics.MutationErrors.Add(1)
			return lifecycle.Record{}, lifecycle.ApplyResult{}, err
		}
	}
	version, err := r.nextVersion(ctx)
	if err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	record := lifecycle.Record{
		Namespace:   []byte(namespace),
		LogicalKey:  []byte(logicalKey),
		State:       lifecycle.StatePresent,
		BlobKey:     blobKey,
		Version:     version,
		AuthorityID: r.authority.AuthorityID(),
	}
	result, err := r.applier.Apply(ctx, []string{lifecycle.LifecycleCapabilityV1}, record, data)
	if err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	r.metrics.MutationsApplied.Add(1)
	return record, result, nil
}

func (r *lifecycleRuntime) delete(ctx context.Context, namespace, logicalKey string) (lifecycle.Record, lifecycle.ApplyResult, error) {
	if r == nil || r.applier == nil {
		return lifecycle.Record{}, lifecycle.ApplyResult{}, errors.New("lifecycle: runtime applier unavailable")
	}
	r.metrics.MutationsStarted.Add(1)
	if err := r.state.ValidateKey([]byte(namespace), []byte(logicalKey)); err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	version, err := r.nextVersion(ctx)
	if err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	record := lifecycle.Record{
		Namespace:   []byte(namespace),
		LogicalKey:  []byte(logicalKey),
		State:       lifecycle.StateDeleted,
		Version:     version,
		AuthorityID: r.authority.AuthorityID(),
	}
	result, err := r.applier.Apply(ctx, []string{lifecycle.LifecycleCapabilityV1}, record, nil)
	if err != nil {
		r.metrics.MutationErrors.Add(1)
		return lifecycle.Record{}, lifecycle.ApplyResult{}, err
	}
	r.metrics.MutationsApplied.Add(1)
	return record, result, nil
}

func (r *lifecycleRuntime) recordsForRawSync(ctx context.Context, peerID string) ([]lifecycle.Record, error) {
	if r == nil || r.journal == nil || r.watermarks == nil {
		return nil, errors.New("lifecycle: raw sync state unavailable")
	}
	records, err := r.journal.Records(ctx)
	if err != nil {
		return nil, err
	}
	watermark := r.watermarks.Watermark(peerID)
	seen := make(map[string]struct{})
	selected := make([]lifecycle.Record, 0, len(records))
	appendRecord := func(record lifecycle.Record) {
		if record.State != lifecycle.StatePresent || record.Version.Compare(watermark) <= 0 {
			return
		}
		key := string(record.BlobKey)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		selected = append(selected, record)
	}
	for _, record := range records {
		appendRecord(record)
	}
	if watermark.Compare(r.journal.Floor()) < 0 {
		checkpoint := r.Snapshot()
		if checkpoint != nil {
			for _, record := range checkpoint.Records {
				appendRecord(record)
			}
		}
	}
	return selected, nil
}

func (r *lifecycleRuntime) waitForVersion(ctx context.Context, version lifecycle.Version, peerCount int) error {
	if r == nil || r.watermarks == nil {
		return errors.New("lifecycle: repair acknowledgements unavailable")
	}
	if peerCount <= 0 {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		acknowledged := 0
		for _, watermark := range r.watermarks.Snapshot() {
			if watermark.Compare(version) >= 0 {
				acknowledged++
			}
		}
		if acknowledged >= peerCount {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *lifecycleRuntime) compact(ctx context.Context) error {
	if r == nil || r.journal == nil || r.state == nil || r.membership == nil || r.watermarks == nil {
		return errors.New("lifecycle: compaction state unavailable")
	}
	target := r.journal.LastVersion()
	if target.Compare(r.journal.Floor()) <= 0 {
		return lifecycle.ErrCompactionNoProgress
	}
	peerWatermarks, err := r.membership.WatermarksAt(ctx, r.watermarks, target)
	if err != nil {
		return err
	}
	checkpoint := lifecycle.Checkpoint{
		Watermark: target,
		Records:   r.state.Snapshot(),
	}
	if err := r.journal.Compact(ctx, lifecycle.CompactionRequest{
		CheckpointPath: r.checkpointPath,
		Watermark:      target,
		Records:        checkpoint.Records,
		Base:           r.Snapshot(),
		PeerWatermarks: peerWatermarks,
	}); err != nil {
		return err
	}
	r.snapshotMu.Lock()
	r.snapshot = &checkpoint
	r.snapshotMu.Unlock()
	return nil
}

func (r *lifecycleRuntime) compactionProgress() lifecycle.MembershipProgress {
	progress := lifecycle.MembershipProgress{}
	if r == nil {
		return progress
	}
	if r.journal != nil {
		progress.Target = r.journal.LastVersion()
	}
	if r.membership == nil || r.watermarks == nil {
		return progress
	}
	current, err := r.membership.Progress(context.Background(), r.watermarks, progress.Target)
	if err != nil {
		return progress
	}
	return current
}

func (r *lifecycleRuntime) compactionStatus(progress lifecycle.MembershipProgress) (blocked bool, reason string) {
	if r == nil || r.membership == nil {
		return true, "membership-unavailable"
	}
	if r.watermarks == nil || r.journal == nil {
		return true, "compaction-state-unavailable"
	}
	if !progress.Configured {
		return true, "membership-missing"
	}
	if progress.Target.Compare(r.journal.Floor()) <= 0 {
		return true, "no-progress"
	}
	if progress.Members != progress.Acknowledged {
		return true, "member-behind"
	}
	return false, ""
}

func (r *lifecycleRuntime) Status() lifecycleStatus {
	if r == nil {
		return lifecycleStatus{Ready: true, Readiness: "raw-only"}
	}
	status := lifecycleStatus{
		Enabled:                 true,
		Ready:                   r.Ready(),
		Readiness:               "ready",
		RepairSessionsActive:    r.metrics.SessionsActive.Load(),
		RepairSessionsStarted:   r.metrics.SessionsStarted.Load(),
		RepairSessionsCompleted: r.metrics.SessionsCompleted.Load(),
	}
	if !status.Ready {
		status.Readiness = "lifecycle-state-unavailable"
	}
	if r.authority != nil {
		status.AuthorityID = r.authority.AuthorityID()
		status.AuthorityVersion = r.authority.Current()
	}
	if r.journal != nil {
		journal := r.journal.Stats()
		status.JournalFloor = journal.Floor
		status.JournalTail = journal.Last
		status.JournalEntries = journal.Entries
		status.JournalBytes = journal.Bytes
	}
	if r.state != nil {
		state := r.state.Stats()
		status.LogicalRecords = state.Records
		status.Tombstones = state.Tombstones
	}
	progress := r.compactionProgress()
	status.MembershipConfigured = progress.Configured
	status.MembershipMembers = progress.Members
	status.MembershipAcknowledged = progress.Acknowledged
	status.MembershipMinimum = progress.Minimum
	status.CompactionTarget = progress.Target
	status.CompactionBlocked, status.CompactionBlockedReason = r.compactionStatus(progress)
	return status
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

func (r *lifecycleRuntime) AttachPeer(ctx context.Context, peer p2p.Peer, maxFrameBytes int, log *slog.Logger, rawSync lifecycleRawRecordSync) {
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
		if previous.active {
			r.metrics.SessionsActive.Add(-1)
		}
	}
	r.sessions[key] = lifecycleSessionEntry{session: session, cancel: cancel, active: true}
	r.metrics.SessionsStarted.Add(1)
	r.metrics.SessionsActive.Add(1)
	r.sessionsWG.Add(1)
	r.sessionsMu.Unlock()

	go func() {
		defer r.sessionsWG.Done()
		defer r.finishSession(key, session)
		if rawSync != nil {
			records, syncErr := r.recordsForRawSync(sessionCtx, remoteID)
			if syncErr == nil {
				syncErr = rawSync(sessionCtx, peer, records)
			}
			if syncErr != nil {
				if !errors.Is(syncErr, context.Canceled) && !errors.Is(syncErr, context.DeadlineExceeded) {
					r.metrics.SessionErrors.Add(1)
					log.Warn("lifecycle raw blob preflight", "remote", peer.RemoteAddr().String(), "err", syncErr)
				}
				return
			}
		}
		err := session.Run(sessionCtx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.metrics.SessionErrors.Add(1)
			log.Warn("lifecycle repair session", "remote", peer.RemoteAddr().String(), "err", err)
		} else if err == nil {
			r.metrics.SessionsCompleted.Add(1)
		}
	}()
}

func (r *lifecycleRuntime) finishSession(key string, session *lifecycle.RepairSession) {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	entry, exists := r.sessions[key]
	if !exists || entry.session != session || !entry.active {
		return
	}
	entry.active = false
	r.sessions[key] = entry
	r.metrics.SessionsActive.Add(-1)
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
		if entry.active {
			r.metrics.SessionsActive.Add(-1)
		}
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
	if r == nil {
		return true
	}
	return r.journal != nil && r.authority != nil && r.watermarks != nil && r.membership != nil &&
		r.coordinator != nil && r.state != nil && r.applier != nil
}

func (r *lifecycleRuntime) Metrics() map[string]int64 {
	if r == nil {
		return map[string]int64{}
	}
	status := r.Status()
	return map[string]int64{
		"lifecycle_enabled":                    1,
		"lifecycle_ready":                      boolMetric(status.Ready),
		"lifecycle_authority_epoch":            int64(status.AuthorityVersion.Epoch),
		"lifecycle_authority_sequence":         int64(status.AuthorityVersion.Sequence),
		"lifecycle_journal_floor_epoch":        int64(status.JournalFloor.Epoch),
		"lifecycle_journal_floor_sequence":     int64(status.JournalFloor.Sequence),
		"lifecycle_journal_tail_epoch":         int64(status.JournalTail.Epoch),
		"lifecycle_journal_tail_sequence":      int64(status.JournalTail.Sequence),
		"lifecycle_journal_entries":            int64(status.JournalEntries),
		"lifecycle_journal_bytes":              status.JournalBytes,
		"lifecycle_logical_records":            int64(status.LogicalRecords),
		"lifecycle_tombstones":                 int64(status.Tombstones),
		"lifecycle_membership_configured":      boolMetric(status.MembershipConfigured),
		"lifecycle_membership_members":         int64(status.MembershipMembers),
		"lifecycle_membership_acknowledged":    int64(status.MembershipAcknowledged),
		"lifecycle_membership_min_epoch":       int64(status.MembershipMinimum.Epoch),
		"lifecycle_membership_min_sequence":    int64(status.MembershipMinimum.Sequence),
		"lifecycle_compaction_target_epoch":    int64(status.CompactionTarget.Epoch),
		"lifecycle_compaction_target_sequence": int64(status.CompactionTarget.Sequence),
		"lifecycle_compaction_blocked":         boolMetric(status.CompactionBlocked),
		"lifecycle_repair_sessions_started":    int64(r.metrics.SessionsStarted.Load()),
		"lifecycle_repair_sessions_completed":  int64(r.metrics.SessionsCompleted.Load()),
		"lifecycle_repair_sessions_active":     r.metrics.SessionsActive.Load(),
		"lifecycle_repair_session_errors":      int64(r.metrics.SessionErrors.Load()),
		"lifecycle_repair_frames_received":     int64(r.metrics.FramesReceived.Load()),
		"lifecycle_repair_frame_errors":        int64(r.metrics.FrameErrors.Load()),
		"lifecycle_mutations_started":          int64(r.metrics.MutationsStarted.Load()),
		"lifecycle_mutations_applied":          int64(r.metrics.MutationsApplied.Load()),
		"lifecycle_mutation_errors":            int64(r.metrics.MutationErrors.Load()),
	}
}

func boolMetric(value bool) int64 {
	if value {
		return 1
	}
	return 0
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
