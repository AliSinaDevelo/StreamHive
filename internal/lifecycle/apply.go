package lifecycle

import (
	"context"
	"errors"
	"sync"

	"github.com/AliSinaDevelo/StreamHive/storage"
)

var (
	// ErrNilApplierState is returned when an applier has no logical state view.
	ErrNilApplierState = errors.New("lifecycle: nil applier state")
	// ErrNilApplierJournal is returned when an applier has no durable journal.
	ErrNilApplierJournal = errors.New("lifecycle: nil applier journal")
	// ErrNilApplierBlobStore is returned when a present record cannot access raw blobs.
	ErrNilApplierBlobStore = errors.New("lifecycle: nil applier blob store")
	// ErrLifecycleBlobMissing is returned when a present record references no available blob.
	ErrLifecycleBlobMissing = errors.New("lifecycle: referenced blob is missing")
)

// Applier makes one capability-gated lifecycle transition durable and deterministic.
type Applier struct {
	mu      sync.Mutex
	blobs   storage.BlobStore
	state   *Store
	journal *Journal
	limits  Limits
}

// NewApplier constructs an applier backed by raw blobs, logical state, and a durable journal.
func NewApplier(blobs storage.BlobStore, state *Store, journal *Journal, limits Limits) (*Applier, error) {
	if state == nil {
		return nil, ErrNilApplierState
	}
	if journal == nil {
		return nil, ErrNilApplierJournal
	}
	return &Applier{
		blobs:   blobs,
		state:   state,
		journal: journal,
		limits:  limits.normalized(),
	}, nil
}

// Apply verifies capability and blob durability, appends the journal, then publishes state.
// suppliedBlob is optional for present records; nil reads the referenced blob from the store.
func (a *Applier) Apply(ctx context.Context, capabilities []string, record Record, suppliedBlob []byte) (ApplyResult, error) {
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	if !HasLifecycleCapability(capabilities) {
		return ApplyResult{}, ErrLifecycleCapabilityRequired
	}
	if err := record.Validate(a.limits); err != nil {
		return ApplyResult{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	classification, err := a.state.Classify(record)
	if err != nil {
		return ApplyResult{}, err
	}
	if classification.Outcome != OutcomeApplied {
		return classification, nil
	}

	if record.State == StatePresent {
		if err := a.ensureBlob(ctx, record.BlobKey, suppliedBlob); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := a.journal.Append(ctx, record); err != nil {
		return ApplyResult{}, err
	}
	return a.state.Apply(record)
}

// ApplySnapshot verifies every referenced blob, durably installs the checkpoint,
// then publishes the complete logical view. The watermark acknowledgement must
// be sent by the caller only after this method returns successfully.
func (a *Applier) ApplySnapshot(ctx context.Context, capabilities []string, snapshot Checkpoint, checkpointPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !HasLifecycleCapability(capabilities) {
		return ErrLifecycleCapabilityRequired
	}
	if checkpointPath == "" {
		return ErrNilCheckpointPath
	}
	normalized, _, err := normalizeCheckpoint(snapshot, a.limits)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, record := range normalized.Records {
		if record.State == StatePresent {
			if err := a.ensureBlob(ctx, record.BlobKey, nil); err != nil {
				return err
			}
		}
	}
	if err := a.journal.InstallSnapshot(ctx, checkpointPath, normalized); err != nil {
		return err
	}
	return a.state.ReplaceSnapshot(normalized.Records)
}

func (a *Applier) ensureBlob(ctx context.Context, key, suppliedBlob []byte) error {
	if a.blobs == nil {
		return ErrNilApplierBlobStore
	}
	data := suppliedBlob
	if data == nil {
		var err error
		data, err = a.blobs.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return errors.Join(ErrLifecycleBlobMissing, err)
		}
		if err != nil {
			return err
		}
	}
	if err := storage.VerifySHA256Key(key, data); err != nil {
		return err
	}
	if suppliedBlob != nil {
		if err := a.blobs.Put(ctx, key, data); err != nil {
			return err
		}
	}
	return nil
}
