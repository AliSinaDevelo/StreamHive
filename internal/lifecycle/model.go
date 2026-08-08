package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxNamespaceBytes   = 128
	DefaultMaxLogicalKeyBytes  = 1024
	DefaultMaxAuthorityIDBytes = 128
	DefaultMaxRecordBytes      = 64 << 10
	DefaultMaxCheckpointBytes  = 64 << 20
	DefaultMaxJournalBytes     = 64 << 20
	DefaultMaxJournalEntries   = 65536
)

var (
	ErrNamespaceEmpty        = errors.New("lifecycle: empty namespace")
	ErrNamespaceTooLarge     = errors.New("lifecycle: namespace too large")
	ErrLogicalKeyEmpty       = errors.New("lifecycle: empty logical key")
	ErrLogicalKeyTooLarge    = errors.New("lifecycle: logical key too large")
	ErrAuthorityEmpty        = errors.New("lifecycle: empty authority")
	ErrAuthorityTooLarge     = errors.New("lifecycle: authority too large")
	ErrAuthorityNotPrintable = errors.New("lifecycle: authority is not printable")
	ErrInvalidState          = errors.New("lifecycle: invalid state")
	ErrInvalidBlobKey        = errors.New("lifecycle: invalid blob key")
	ErrZeroVersion           = errors.New("lifecycle: zero version")
	ErrRecordTooLarge        = errors.New("lifecycle: record too large")
	ErrConflict              = errors.New("lifecycle: same-version record conflict")
	ErrDuplicateLogicalKey   = errors.New("lifecycle: duplicate logical key")
	ErrNilApply              = errors.New("lifecycle: nil apply function")
)

// Limits bounds lifecycle metadata before it is persisted or exchanged.
type Limits struct {
	MaxNamespaceBytes   int
	MaxLogicalKeyBytes  int
	MaxAuthorityIDBytes int
	MaxRecordBytes      int
	MaxCheckpointBytes  int
}

func (l Limits) normalized() Limits {
	if l.MaxNamespaceBytes <= 0 {
		l.MaxNamespaceBytes = DefaultMaxNamespaceBytes
	}
	if l.MaxLogicalKeyBytes <= 0 {
		l.MaxLogicalKeyBytes = DefaultMaxLogicalKeyBytes
	}
	if l.MaxAuthorityIDBytes <= 0 {
		l.MaxAuthorityIDBytes = DefaultMaxAuthorityIDBytes
	}
	if l.MaxRecordBytes <= 0 {
		l.MaxRecordBytes = DefaultMaxRecordBytes
	}
	if l.MaxCheckpointBytes <= 0 {
		l.MaxCheckpointBytes = DefaultMaxCheckpointBytes
	}
	return l
}

// Version is the total-order token assigned by a namespace authority.
type Version struct {
	Epoch    uint64 `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

// Compare compares versions lexicographically by epoch and sequence.
func (v Version) Compare(other Version) int {
	if v.Epoch < other.Epoch || (v.Epoch == other.Epoch && v.Sequence < other.Sequence) {
		return -1
	}
	if v.Epoch > other.Epoch || (v.Epoch == other.Epoch && v.Sequence > other.Sequence) {
		return 1
	}
	return 0
}

func (v Version) IsZero() bool {
	return v == Version{}
}

// LifecycleState is the logical state of one application key.
type LifecycleState string

const (
	StatePresent LifecycleState = "present"
	StateDeleted LifecycleState = "deleted"
)

// Record is a logical lifecycle mutation. BlobKey is present only for a present record.
type Record struct {
	Namespace   []byte         `json:"namespace"`
	LogicalKey  []byte         `json:"logical_key"`
	State       LifecycleState `json:"state"`
	BlobKey     []byte         `json:"blob_key,omitempty"`
	Version     Version        `json:"version"`
	AuthorityID string         `json:"authority_id"`
}

// Validate checks bounds and the invariants required before persistence.
func (r Record) Validate(limits Limits) error {
	limits = limits.normalized()
	if len(r.Namespace) == 0 {
		return ErrNamespaceEmpty
	}
	if len(r.Namespace) > limits.MaxNamespaceBytes {
		return ErrNamespaceTooLarge
	}
	if len(r.LogicalKey) == 0 {
		return ErrLogicalKeyEmpty
	}
	if len(r.LogicalKey) > limits.MaxLogicalKeyBytes {
		return ErrLogicalKeyTooLarge
	}
	if r.AuthorityID == "" {
		return ErrAuthorityEmpty
	}
	if len(r.AuthorityID) > limits.MaxAuthorityIDBytes {
		return ErrAuthorityTooLarge
	}
	if !utf8.ValidString(r.AuthorityID) {
		return ErrAuthorityNotPrintable
	}
	for _, character := range r.AuthorityID {
		if !unicode.IsPrint(character) {
			return ErrAuthorityNotPrintable
		}
	}
	if r.Version.IsZero() {
		return ErrZeroVersion
	}
	switch r.State {
	case StatePresent:
		if len(r.BlobKey) != sha256.Size {
			return ErrInvalidBlobKey
		}
	case StateDeleted:
		if len(r.BlobKey) != 0 {
			return ErrInvalidBlobKey
		}
	default:
		return ErrInvalidState
	}

	encoded, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("lifecycle: encode record: %w", err)
	}
	if len(encoded) > limits.MaxRecordBytes {
		return ErrRecordTooLarge
	}
	return nil
}

func (r Record) clone() Record {
	r.Namespace = append([]byte(nil), r.Namespace...)
	r.LogicalKey = append([]byte(nil), r.LogicalKey...)
	r.BlobKey = append([]byte(nil), r.BlobKey...)
	return r
}

func (r Record) equal(other Record) bool {
	return bytes.Equal(r.Namespace, other.Namespace) &&
		bytes.Equal(r.LogicalKey, other.LogicalKey) &&
		r.State == other.State &&
		bytes.Equal(r.BlobKey, other.BlobKey) &&
		r.Version == other.Version &&
		r.AuthorityID == other.AuthorityID
}

func (r Record) bodyEqual(other Record) bool {
	return r.State == other.State &&
		bytes.Equal(r.BlobKey, other.BlobKey) &&
		r.AuthorityID == other.AuthorityID
}

type logicalKey struct {
	namespace  string
	logicalKey string
}

func (l Limits) validateLookup(namespace, key []byte) error {
	if len(namespace) == 0 {
		return ErrNamespaceEmpty
	}
	if len(namespace) > l.normalized().MaxNamespaceBytes {
		return ErrNamespaceTooLarge
	}
	if len(key) == 0 {
		return ErrLogicalKeyEmpty
	}
	if len(key) > l.normalized().MaxLogicalKeyBytes {
		return ErrLogicalKeyTooLarge
	}
	return nil
}

// ApplyOutcome classifies a record application.
type ApplyOutcome string

const (
	OutcomeApplied   ApplyOutcome = "applied"
	OutcomeDuplicate ApplyOutcome = "duplicate"
	OutcomeStale     ApplyOutcome = "stale"
)

// ApplyResult reports the deterministic outcome for one record.
type ApplyResult struct {
	Outcome ApplyOutcome
	Version Version
}

// Store is an in-memory logical state view used by the journal and future replicas.
type Store struct {
	mu      sync.RWMutex
	limits  Limits
	records map[logicalKey]Record
}

// NewStore returns an empty lifecycle state view.
func NewStore(limits Limits) *Store {
	return &Store{
		limits:  limits.normalized(),
		records: make(map[logicalKey]Record),
	}
}

// Classify validates a record and reports its outcome without changing state.
func (s *Store) Classify(record Record) (ApplyResult, error) {
	if err := record.Validate(s.limits); err != nil {
		return ApplyResult{}, err
	}
	key := logicalKey{namespace: string(record.Namespace), logicalKey: string(record.LogicalKey)}
	s.mu.RLock()
	defer s.mu.RUnlock()
	previous, exists := s.records[key]
	return classifyRecord(record, previous, exists)
}

// Apply applies a record according to version and same-token conflict rules.
func (s *Store) Apply(record Record) (ApplyResult, error) {
	if err := record.Validate(s.limits); err != nil {
		return ApplyResult{}, err
	}
	key := logicalKey{namespace: string(record.Namespace), logicalKey: string(record.LogicalKey)}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.records[key]
	result, err := classifyRecord(record, previous, exists)
	if err != nil {
		return ApplyResult{}, err
	}
	if result.Outcome == OutcomeApplied {
		s.records[key] = record.clone()
	}
	return result, nil
}

func classifyRecord(record, previous Record, exists bool) (ApplyResult, error) {
	if !exists {
		return ApplyResult{Outcome: OutcomeApplied, Version: record.Version}, nil
	}
	switch comparison := record.Version.Compare(previous.Version); {
	case comparison > 0:
		return ApplyResult{Outcome: OutcomeApplied, Version: record.Version}, nil
	case comparison < 0:
		return ApplyResult{Outcome: OutcomeStale, Version: previous.Version}, nil
	case record.bodyEqual(previous):
		return ApplyResult{Outcome: OutcomeDuplicate, Version: previous.Version}, nil
	default:
		return ApplyResult{}, ErrConflict
	}
}

// Get returns a copy of the current record for a logical key.
func (s *Store) Get(namespace, key []byte) (Record, bool, error) {
	if err := s.limits.validateLookup(namespace, key); err != nil {
		return Record{}, false, err
	}
	lookup := logicalKey{namespace: string(namespace), logicalKey: string(key)}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[lookup]
	if !ok {
		return Record{}, false, nil
	}
	return record.clone(), true, nil
}

// Snapshot returns records in deterministic namespace/key order.
func (s *Store) Snapshot() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record.clone())
	}
	sort.Slice(records, func(i, j int) bool {
		if comparison := bytes.Compare(records[i].Namespace, records[j].Namespace); comparison != 0 {
			return comparison < 0
		}
		return bytes.Compare(records[i].LogicalKey, records[j].LogicalKey) < 0
	})
	return records
}

func normalizeCheckpointRecords(records []Record, watermark Version, limits Limits) ([]Record, error) {
	limits = limits.normalized()
	if watermark.IsZero() && len(records) > 0 {
		return nil, ErrZeroVersion
	}
	out := make([]Record, 0, len(records))
	for _, record := range records {
		if err := record.Validate(limits); err != nil {
			return nil, err
		}
		if record.Version.Compare(watermark) > 0 {
			return nil, fmt.Errorf("lifecycle: record exceeds checkpoint watermark: %w", ErrConflict)
		}
		out = append(out, record.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if comparison := bytes.Compare(out[i].Namespace, out[j].Namespace); comparison != 0 {
			return comparison < 0
		}
		return bytes.Compare(out[i].LogicalKey, out[j].LogicalKey) < 0
	})
	for i := 1; i < len(out); i++ {
		if bytes.Equal(out[i-1].Namespace, out[i].Namespace) && bytes.Equal(out[i-1].LogicalKey, out[i].LogicalKey) {
			return nil, ErrDuplicateLogicalKey
		}
	}
	return out, nil
}

func recordsEqual(left, right []Record) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !left[i].equal(right[i]) {
			return false
		}
	}
	return true
}
