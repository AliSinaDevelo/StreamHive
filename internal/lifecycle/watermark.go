package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultMaxRepairPeers bounds the durable acknowledgement map.
	DefaultMaxRepairPeers = 1024
	// DefaultMaxRepairPeerIDBytes bounds one durable peer identity.
	DefaultMaxRepairPeerIDBytes = 128
	// DefaultMaxRepairWatermarkBytes bounds the complete watermark file envelope.
	DefaultMaxRepairWatermarkBytes = 1 << 20
)

var (
	// ErrNilWatermarkPath is returned when no durable acknowledgement path is configured.
	ErrNilWatermarkPath = errors.New("lifecycle: empty watermark path")
	// ErrNilWatermarkBook is returned when a watermark operation has no book.
	ErrNilWatermarkBook = errors.New("lifecycle: nil watermark book")
	// ErrWatermarkCorrupt is returned for malformed or structurally invalid state.
	ErrWatermarkCorrupt = errors.New("lifecycle: corrupt repair watermarks")
	// ErrWatermarkChecksum is returned when durable acknowledgement state fails its CRC.
	ErrWatermarkChecksum = errors.New("lifecycle: repair watermark checksum mismatch")
	// ErrWatermarkLimit is returned when the peer map or encoded state exceeds its bounds.
	ErrWatermarkLimit = errors.New("lifecycle: repair watermark limit exceeded")
	// ErrWatermarkPeerInvalid is returned for an empty, invalid, or oversized peer ID.
	ErrWatermarkPeerInvalid = errors.New("lifecycle: invalid repair peer identity")
	// ErrWatermarkRegression is returned when an acknowledgement moves backward.
	ErrWatermarkRegression = errors.New("lifecycle: repair watermark regressed")
	// ErrNilRepairWatermarks is returned when a coordinator has no durable watermark book.
	ErrNilRepairWatermarks = errors.New("lifecycle: nil repair watermarks")
	// ErrRepairAcknowledgement is returned when a peer acknowledges beyond the local journal.
	ErrRepairAcknowledgement = errors.New("lifecycle: repair acknowledgement exceeds journal")
)

// WatermarkOptions bounds durable per-peer acknowledgement state.
type WatermarkOptions struct {
	MaxPeers       int
	MaxPeerIDBytes int
	MaxBytes       int
}

func (o WatermarkOptions) normalized() WatermarkOptions {
	if o.MaxPeers <= 0 {
		o.MaxPeers = DefaultMaxRepairPeers
	}
	if o.MaxPeerIDBytes <= 0 {
		o.MaxPeerIDBytes = DefaultMaxRepairPeerIDBytes
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxRepairWatermarkBytes
	}
	return o
}

type watermarkState struct {
	Watermarks map[string]Version `json:"watermarks"`
}

// WatermarkBook durably stores monotonic lifecycle acknowledgements by peer.
// Every successful mutation is written through an envelope and atomic rename
// before becoming visible to readers.
type WatermarkBook struct {
	mu         sync.RWMutex
	path       string
	maxPeers   int
	maxPeerID  int
	maxBytes   int
	watermarks map[string]Version
}

// OpenWatermarkBook opens a durable acknowledgement map, or an empty one when
// the path has not been created yet.
func OpenWatermarkBook(path string, options WatermarkOptions) (*WatermarkBook, error) {
	if path == "" {
		return nil, ErrNilWatermarkPath
	}
	options = options.normalized()
	book := &WatermarkBook{
		path:       path,
		maxPeers:   options.MaxPeers,
		maxPeerID:  options.MaxPeerIDBytes,
		maxBytes:   options.MaxBytes,
		watermarks: make(map[string]Version),
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return book, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrWatermarkCorrupt
	}
	if len(data) > book.maxBytes {
		return nil, ErrWatermarkLimit
	}
	payload, next, err := decodeEnvelope(data, 0, book.maxBytes, ErrWatermarkCorrupt, ErrWatermarkChecksum)
	if err != nil {
		return nil, err
	}
	if next != len(data) {
		return nil, ErrWatermarkCorrupt
	}
	state, err := decodeWatermarkState(payload)
	if err != nil {
		return nil, err
	}
	if err := book.validateState(state); err != nil {
		return nil, err
	}
	for peer, version := range state.Watermarks {
		book.watermarks[peer] = version
	}
	return book, nil
}

// Acknowledge durably advances one peer's watermark. Zero is the implicit
// initial watermark and does not create a peer entry.
func (b *WatermarkBook) Acknowledge(ctx context.Context, peer string, version Version) error {
	if b == nil {
		return ErrNilWatermarkBook
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validatePeer(peer); err != nil {
		return err
	}
	if version.IsZero() {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.watermarks[peer]
	switch comparison := version.Compare(current); {
	case comparison < 0:
		return ErrWatermarkRegression
	case comparison == 0 && exists:
		return nil
	}
	if !exists && len(b.watermarks) >= b.maxPeers {
		return ErrWatermarkLimit
	}
	next := cloneWatermarks(b.watermarks)
	next[peer] = version
	if err := b.persistLocked(ctx, next); err != nil {
		return err
	}
	b.watermarks = next
	return nil
}

// Forget removes one peer acknowledgement durably. It is idempotent for an
// unknown peer and is intended for explicit membership removal by an operator.
func (b *WatermarkBook) Forget(ctx context.Context, peer string) error {
	if b == nil {
		return ErrNilWatermarkBook
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validatePeer(peer); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.watermarks[peer]; !exists {
		return nil
	}
	next := cloneWatermarks(b.watermarks)
	delete(next, peer)
	if err := b.persistLocked(ctx, next); err != nil {
		return err
	}
	b.watermarks = next
	return nil
}

// Watermark returns the acknowledged watermark for peer, or zero when unknown.
func (b *WatermarkBook) Watermark(peer string) Version {
	if b == nil {
		return Version{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.watermarks[peer]
}

// Snapshot returns an owned copy of all durable peer acknowledgements.
func (b *WatermarkBook) Snapshot() map[string]Version {
	if b == nil {
		return map[string]Version{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return cloneWatermarks(b.watermarks)
}

func (b *WatermarkBook) validatePeer(peer string) error {
	if peer == "" || len(peer) > b.maxPeerID || !utf8.ValidString(peer) {
		return ErrWatermarkPeerInvalid
	}
	for _, character := range peer {
		if !unicode.IsPrint(character) {
			return ErrWatermarkPeerInvalid
		}
	}
	return nil
}

func (b *WatermarkBook) validateState(state watermarkState) error {
	if state.Watermarks == nil {
		return ErrWatermarkCorrupt
	}
	if len(state.Watermarks) > b.maxPeers {
		return ErrWatermarkLimit
	}
	for peer, version := range state.Watermarks {
		if err := b.validatePeer(peer); err != nil {
			return errors.Join(ErrWatermarkCorrupt, err)
		}
		if version.IsZero() {
			return ErrWatermarkCorrupt
		}
	}
	return nil
}

func (b *WatermarkBook) persistLocked(ctx context.Context, watermarks map[string]Version) error {
	payload, err := json.Marshal(watermarkState{Watermarks: watermarks})
	if err != nil {
		return errors.Join(ErrWatermarkCorrupt, err)
	}
	envelope, err := encodeEnvelope(payload)
	if err != nil {
		return errors.Join(ErrWatermarkCorrupt, err)
	}
	if len(envelope) > b.maxBytes {
		return ErrWatermarkLimit
	}
	return atomicWrite(ctx, b.path, envelope)
}

func decodeWatermarkState(payload []byte) (watermarkState, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state watermarkState
	if err := decoder.Decode(&state); err != nil {
		return watermarkState{}, errors.Join(ErrWatermarkCorrupt, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return watermarkState{}, ErrWatermarkCorrupt
		}
		return watermarkState{}, errors.Join(ErrWatermarkCorrupt, err)
	}
	return state, nil
}

func cloneWatermarks(watermarks map[string]Version) map[string]Version {
	clone := make(map[string]Version, len(watermarks))
	for peer, version := range watermarks {
		clone[peer] = version
	}
	return clone
}

// RepairCoordinator connects durable peer watermarks to bounded journal plans.
type RepairCoordinator struct {
	journal    *Journal
	watermarks *WatermarkBook
	limits     RepairLimits
}

// NewRepairCoordinator creates a planner with durable peer acknowledgement state.
func NewRepairCoordinator(journal *Journal, watermarks *WatermarkBook, limits RepairLimits) (*RepairCoordinator, error) {
	if journal == nil {
		return nil, ErrNilRepairJournal
	}
	if watermarks == nil {
		return nil, ErrNilRepairWatermarks
	}
	return &RepairCoordinator{
		journal:    journal,
		watermarks: watermarks,
		limits:     limits.normalized(),
	}, nil
}

// Plan chooses the next bounded repair payload from the peer's durable watermark.
func (c *RepairCoordinator) Plan(ctx context.Context, peer string, snapshot *Checkpoint) (RepairPlan, error) {
	if c == nil {
		return RepairPlan{}, ErrNilRepairWatermarks
	}
	if err := ctx.Err(); err != nil {
		return RepairPlan{}, err
	}
	if err := c.watermarks.validatePeer(peer); err != nil {
		return RepairPlan{}, err
	}
	return PlanRepair(ctx, c.journal, c.watermarks.Watermark(peer), snapshot, c.limits)
}

// Acknowledge persists a peer's applied watermark after ensuring it exists in
// the local journal. It never advances beyond the source's durable tail.
func (c *RepairCoordinator) Acknowledge(ctx context.Context, peer string, version Version) error {
	if c == nil {
		return ErrNilRepairWatermarks
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.watermarks.validatePeer(peer); err != nil {
		return err
	}
	if !version.IsZero() && version.Compare(c.journal.LastVersion()) > 0 {
		return ErrRepairAcknowledgement
	}
	return c.watermarks.Acknowledge(ctx, peer, version)
}
