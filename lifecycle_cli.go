package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
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
	journal        *lifecycle.Journal
	watermarks     *lifecycle.WatermarkBook
	coordinator    *lifecycle.RepairCoordinator
	state          *lifecycle.Store
	applier        *lifecycle.Applier

	snapshotMu sync.RWMutex
	snapshot   *lifecycle.Checkpoint
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
		journal:        journal,
		watermarks:     watermarks,
		coordinator:    coordinator,
		state:          state,
		applier:        applier,
	}
	if hasCheckpoint {
		runtime.snapshot = &checkpoint
	}
	closeJournal = false
	return runtime, nil
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
