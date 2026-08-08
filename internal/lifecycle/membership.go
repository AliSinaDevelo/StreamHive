package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultMaxMembershipPeers bounds the explicitly configured replica set.
	DefaultMaxMembershipPeers = DefaultMaxRepairPeers
	// DefaultMaxMembershipPeerIDBytes bounds one configured replica identity.
	DefaultMaxMembershipPeerIDBytes = DefaultMaxRepairPeerIDBytes
	// DefaultMaxMembershipBytes bounds the complete membership file envelope.
	DefaultMaxMembershipBytes = DefaultMaxRepairWatermarkBytes
)

var (
	// ErrNilMembershipPath is returned when no durable membership path is configured.
	ErrNilMembershipPath = errors.New("lifecycle: empty membership path")
	// ErrNilMembershipBook is returned when a membership operation has no book.
	ErrNilMembershipBook = errors.New("lifecycle: nil membership book")
	// ErrMembershipNotConfigured is returned when no operator-authored membership exists.
	ErrMembershipNotConfigured = errors.New("lifecycle: membership is not configured")
	// ErrMembershipCorrupt is returned for malformed or structurally invalid state.
	ErrMembershipCorrupt = errors.New("lifecycle: corrupt membership")
	// ErrMembershipChecksum is returned when durable membership state fails its CRC.
	ErrMembershipChecksum = errors.New("lifecycle: membership checksum mismatch")
	// ErrMembershipLimit is returned when the membership set or encoded state exceeds bounds.
	ErrMembershipLimit = errors.New("lifecycle: membership limit exceeded")
	// ErrMembershipPeerInvalid is returned for an empty, invalid, or oversized identity.
	ErrMembershipPeerInvalid = errors.New("lifecycle: invalid membership peer identity")
	// ErrMembershipDuplicate is returned when an operator configures one identity twice.
	ErrMembershipDuplicate = errors.New("lifecycle: duplicate membership peer identity")
	// ErrMembershipWatermarkInvalid is returned for an unusable compaction target.
	ErrMembershipWatermarkInvalid = errors.New("lifecycle: invalid membership watermark")
	// ErrMembershipBehind is returned when a configured member lacks sufficient evidence.
	ErrMembershipBehind = errors.New("lifecycle: membership peer is behind compaction watermark")
)

// MembershipOptions bounds durable operator-authored membership state.
type MembershipOptions struct {
	MaxPeers       int
	MaxPeerIDBytes int
	MaxBytes       int
}

func (o MembershipOptions) normalized() MembershipOptions {
	if o.MaxPeers <= 0 {
		o.MaxPeers = DefaultMaxMembershipPeers
	}
	if o.MaxPeerIDBytes <= 0 {
		o.MaxPeerIDBytes = DefaultMaxMembershipPeerIDBytes
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxMembershipBytes
	}
	return o
}

type membershipState struct {
	Members []string `json:"members"`
}

// MembershipBook durably stores an operator-authored set of replica identities.
// A missing file is distinct from an explicitly persisted empty set so callers can
// fail closed when compaction has no configured safety fence.
type MembershipBook struct {
	mu         sync.RWMutex
	path       string
	maxPeers   int
	maxPeerID  int
	maxBytes   int
	configured bool
	members    []string
}

// OpenMembershipBook opens a durable membership set, or an unconfigured empty book
// when the path has not been created yet.
func OpenMembershipBook(path string, options MembershipOptions) (*MembershipBook, error) {
	if path == "" {
		return nil, ErrNilMembershipPath
	}
	options = options.normalized()
	book := &MembershipBook{
		path:      path,
		maxPeers:  options.MaxPeers,
		maxPeerID: options.MaxPeerIDBytes,
		maxBytes:  options.MaxBytes,
		members:   []string{},
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return book, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrMembershipCorrupt
	}
	if len(data) > book.maxBytes {
		return nil, ErrMembershipLimit
	}
	payload, next, err := decodeEnvelope(data, 0, book.maxBytes, ErrMembershipCorrupt, ErrMembershipChecksum)
	if err != nil {
		return nil, err
	}
	if next != len(data) {
		return nil, ErrMembershipCorrupt
	}
	state, err := decodeMembershipState(payload)
	if err != nil {
		return nil, err
	}
	members, err := book.normalizeMembers(state.Members)
	if err != nil {
		return nil, errors.Join(ErrMembershipCorrupt, err)
	}
	book.members = members
	book.configured = true
	return book, nil
}

// Replace durably replaces the operator-authored membership set. An empty slice
// is valid and creates an explicit empty fence distinct from a missing file.
func (b *MembershipBook) Replace(ctx context.Context, members []string) error {
	if b == nil {
		return ErrNilMembershipBook
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := b.normalizeMembers(members)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(membershipState{Members: normalized})
	if err != nil {
		return errors.Join(ErrMembershipCorrupt, err)
	}
	envelope, err := encodeEnvelope(payload)
	if err != nil {
		return errors.Join(ErrMembershipCorrupt, err)
	}
	if len(envelope) > b.maxBytes {
		return ErrMembershipLimit
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := atomicWrite(ctx, b.path, envelope); err != nil {
		return err
	}
	b.members = append([]string(nil), normalized...)
	if b.members == nil {
		b.members = []string{}
	}
	b.configured = true
	return nil
}

// Configured reports whether an operator-authored membership file exists.
func (b *MembershipBook) Configured() bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.configured
}

// Snapshot returns an owned, stable copy of the configured identities.
func (b *MembershipBook) Snapshot() []string {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.members...)
}

// WatermarksAt checks every configured member's durable acknowledgement at a
// compaction target and returns the ordered evidence required by Journal.Compact.
func (b *MembershipBook) WatermarksAt(ctx context.Context, watermarks *WatermarkBook, target Version) ([]Version, error) {
	if b == nil {
		return nil, ErrNilMembershipBook
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if watermarks == nil {
		return nil, ErrNilWatermarkBook
	}
	if target.IsZero() {
		return nil, ErrMembershipWatermarkInvalid
	}
	b.mu.RLock()
	configured := b.configured
	members := append([]string(nil), b.members...)
	b.mu.RUnlock()
	if !configured {
		return nil, ErrMembershipNotConfigured
	}
	versions := make([]Version, 0, len(members))
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		version := watermarks.Watermark(member)
		if version.Compare(target) < 0 {
			return nil, errors.Join(ErrMembershipBehind, fmt.Errorf("member %q acknowledged %v before %v", member, version, target))
		}
		versions = append(versions, version)
	}
	return versions, nil
}

func (b *MembershipBook) normalizeMembers(members []string) ([]string, error) {
	if len(members) > b.maxPeers {
		return nil, ErrMembershipLimit
	}
	normalized := append([]string(nil), members...)
	for _, member := range normalized {
		if member == "" || len(member) > b.maxPeerID || !utf8.ValidString(member) {
			return nil, ErrMembershipPeerInvalid
		}
		for _, character := range member {
			if !unicode.IsPrint(character) {
				return nil, ErrMembershipPeerInvalid
			}
		}
	}
	sort.Strings(normalized)
	for index := 1; index < len(normalized); index++ {
		if normalized[index] == normalized[index-1] {
			return nil, ErrMembershipDuplicate
		}
	}
	if normalized == nil {
		normalized = []string{}
	}
	return normalized, nil
}

func decodeMembershipState(payload []byte) (membershipState, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state membershipState
	if err := decoder.Decode(&state); err != nil {
		return membershipState{}, errors.Join(ErrMembershipCorrupt, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return membershipState{}, ErrMembershipCorrupt
		}
		return membershipState{}, errors.Join(ErrMembershipCorrupt, err)
	}
	return state, nil
}
