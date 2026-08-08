package lifecycle

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"sync"
)

const DefaultMaxAuthorityStateBytes = 1 << 20

var (
	ErrNilAuthorityPath   = errors.New("lifecycle: empty authority path")
	ErrAuthorityCorrupt   = errors.New("lifecycle: corrupt authority state")
	ErrAuthorityChecksum  = errors.New("lifecycle: authority checksum mismatch")
	ErrAuthorityLimit     = errors.New("lifecycle: authority state limit exceeded")
	ErrAuthorityMismatch  = errors.New("lifecycle: authority identity mismatch")
	ErrAuthorityVersion   = errors.New("lifecycle: invalid authority version")
	ErrAuthorityExhausted = errors.New("lifecycle: authority version space exhausted")
)

// AuthorityOptions bounds durable local version state and lets recovery adopt
// a newer journal tail before the next local mutation is allocated.
type AuthorityOptions struct {
	Limits   Limits
	MaxBytes int
	Observed Version
}

func (o AuthorityOptions) normalized() AuthorityOptions {
	o.Limits = o.Limits.normalized()
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxAuthorityStateBytes
	}
	return o
}

type authorityState struct {
	AuthorityID string  `json:"authority_id"`
	Version     Version `json:"version"`
}

// Authority durably allocates one total-order version stream for a configured
// operator-fenced authority identity. A failed mutation may consume a token;
// it must never be reused.
type Authority struct {
	mu          sync.Mutex
	path        string
	authorityID string
	limits      Limits
	maxBytes    int
	current     Version
}

// OpenAuthority opens or creates the durable local allocator. The observed
// version is adopted when it is newer than the stored allocator state.
func OpenAuthority(ctx context.Context, path, authorityID string, options AuthorityOptions) (*Authority, error) {
	if path == "" {
		return nil, ErrNilAuthorityPath
	}
	options = options.normalized()
	if err := validateAuthorityID(authorityID, options.Limits); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority := &Authority{
		path:        path,
		authorityID: authorityID,
		limits:      options.Limits,
		maxBytes:    options.MaxBytes,
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		authority.current, err = initialAuthorityVersion(options.Observed)
		if err != nil {
			return nil, err
		}
		if err := authority.persistLocked(ctx); err != nil {
			return nil, err
		}
		return authority, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrAuthorityCorrupt
	}
	if len(data) > authority.maxBytes {
		return nil, ErrAuthorityLimit
	}
	payload, next, err := decodeEnvelope(data, 0, authority.maxBytes, ErrAuthorityCorrupt, ErrAuthorityChecksum)
	if err != nil {
		return nil, err
	}
	if next != len(data) {
		return nil, ErrAuthorityCorrupt
	}
	state, err := decodeAuthorityState(payload)
	if err != nil {
		return nil, err
	}
	if state.AuthorityID != authorityID {
		return nil, ErrAuthorityMismatch
	}
	if err := validateAuthorityVersion(state.Version); err != nil {
		return nil, err
	}
	authority.current = state.Version
	if options.Observed.Compare(authority.current) > 0 {
		authority.current = options.Observed
		if authority.current.Epoch == 0 {
			return nil, ErrAuthorityVersion
		}
		if err := authority.persistLocked(ctx); err != nil {
			return nil, err
		}
	}
	return authority, nil
}

// Next durably allocates a token strictly after both the stored token and the
// observed journal tail.
func (a *Authority) Next(ctx context.Context, observed Version) (Version, error) {
	if a == nil {
		return Version{}, ErrAuthorityVersion
	}
	if err := ctx.Err(); err != nil {
		return Version{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if observed.Compare(a.current) > 0 {
		if observed.Epoch == 0 {
			return Version{}, ErrAuthorityVersion
		}
		a.current = observed
	}
	next, err := incrementAuthorityVersion(a.current)
	if err != nil {
		return Version{}, err
	}
	a.current = next
	if err := a.persistLocked(ctx); err != nil {
		return Version{}, err
	}
	return next, nil
}

// Current returns the last durably allocated token, or the initial epoch with
// sequence zero when the allocator has not issued a mutation yet.
func (a *Authority) Current() Version {
	if a == nil {
		return Version{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

func initialAuthorityVersion(observed Version) (Version, error) {
	if observed.Epoch != 0 {
		return observed, nil
	}
	var raw [8]byte
	if _, err := io.ReadFull(cryptorand.Reader, raw[:]); err != nil {
		return Version{}, err
	}
	epoch := binary.BigEndian.Uint64(raw[:])
	if epoch == 0 {
		epoch = 1
	}
	return Version{Epoch: epoch}, nil
}

func incrementAuthorityVersion(current Version) (Version, error) {
	if current.Epoch == 0 {
		return Version{}, ErrAuthorityVersion
	}
	if current.Sequence < math.MaxUint64 {
		return Version{Epoch: current.Epoch, Sequence: current.Sequence + 1}, nil
	}
	if current.Epoch == math.MaxUint64 {
		return Version{}, ErrAuthorityExhausted
	}
	return Version{Epoch: current.Epoch + 1, Sequence: 1}, nil
}

func validateAuthorityVersion(version Version) error {
	if version.Epoch == 0 {
		return ErrAuthorityVersion
	}
	return nil
}

func (a *Authority) persistLocked(ctx context.Context) error {
	payload, err := json.Marshal(authorityState{AuthorityID: a.authorityID, Version: a.current})
	if err != nil {
		return errors.Join(ErrAuthorityCorrupt, err)
	}
	envelope, err := encodeEnvelope(payload)
	if err != nil {
		return errors.Join(ErrAuthorityCorrupt, err)
	}
	if len(envelope) > a.maxBytes {
		return ErrAuthorityLimit
	}
	return atomicWrite(ctx, a.path, envelope)
}

func decodeAuthorityState(payload []byte) (authorityState, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state authorityState
	if err := decoder.Decode(&state); err != nil {
		return authorityState{}, errors.Join(ErrAuthorityCorrupt, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return authorityState{}, ErrAuthorityCorrupt
		}
		return authorityState{}, errors.Join(ErrAuthorityCorrupt, err)
	}
	return state, nil
}
