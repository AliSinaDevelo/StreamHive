package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
)

var (
	// ErrInventoryKeyEmpty reports a malformed inventory entry.
	ErrInventoryKeyEmpty = errors.New("storage: inventory contains empty key")
	// ErrInventoryPagerInvalid reports a pager that violates BlobKeyPager's cursor contract.
	ErrInventoryPagerInvalid = errors.New("storage: invalid inventory pager")
)

// InventorySummary is a live aggregate of the keys currently visible to a lister.
// Digest is SHA-256 over each key's uint32 big-endian length followed by its bytes,
// in bytewise key order.
type InventorySummary struct {
	KeyCount int
	KeyBytes int64
	Digest   [sha256.Size]byte
}

// DigestHex returns the lowercase hexadecimal inventory fingerprint.
func (s InventorySummary) DigestHex() string {
	return hex.EncodeToString(s.Digest[:])
}

// SummarizeInventory computes a deterministic aggregate without changing the
// add-only replication protocol. Native pagers are consumed one bounded page at
// a time; older listers use their complete-list compatibility path.
func SummarizeInventory(ctx context.Context, lister BlobKeyLister) (InventorySummary, error) {
	if lister == nil {
		return InventorySummary{}, errors.New("storage: nil inventory lister")
	}
	if err := ctx.Err(); err != nil {
		return InventorySummary{}, err
	}

	if pager, ok := lister.(BlobKeyPager); ok {
		return summarizePagedInventory(ctx, pager)
	}
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return InventorySummary{}, err
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i], keys[j]) < 0
	})
	return summarizeInventoryKeys(ctx, keys)
}

func summarizePagedInventory(ctx context.Context, pager BlobKeyPager) (InventorySummary, error) {
	var summary InventorySummary
	digest := sha256.New()
	var cursor []byte
	hasCursor := false
	for {
		if err := ctx.Err(); err != nil {
			return InventorySummary{}, err
		}
		keys, next, err := pager.ListKeyPage(ctx, cursor, DefaultKeyPageSize)
		if err != nil {
			return InventorySummary{}, err
		}
		if len(keys) == 0 {
			if len(next) != 0 {
				return InventorySummary{}, fmt.Errorf("%w: empty page returned cursor", ErrInventoryPagerInvalid)
			}
			copy(summary.Digest[:], digest.Sum(nil))
			return summary, nil
		}
		if len(keys) > DefaultKeyPageSize || len(next) == 0 || !bytes.Equal(next, keys[len(keys)-1]) {
			return InventorySummary{}, fmt.Errorf("%w: page size or cursor mismatch", ErrInventoryPagerInvalid)
		}
		for i, key := range keys {
			if err := ctx.Err(); err != nil {
				return InventorySummary{}, err
			}
			if len(key) == 0 {
				return InventorySummary{}, ErrInventoryKeyEmpty
			}
			if hasCursor && bytes.Compare(key, cursor) <= 0 {
				return InventorySummary{}, fmt.Errorf("%w: page does not advance", ErrInventoryPagerInvalid)
			}
			if i > 0 && bytes.Compare(key, keys[i-1]) <= 0 {
				return InventorySummary{}, fmt.Errorf("%w: page is not strictly sorted", ErrInventoryPagerInvalid)
			}
		}
		for _, key := range keys {
			if err := appendInventoryKey(ctx, &summary, digest, key); err != nil {
				return InventorySummary{}, err
			}
		}
		cursor = append(cursor[:0], next...)
		hasCursor = true
	}
}

func summarizeInventoryKeys(ctx context.Context, keys [][]byte) (InventorySummary, error) {
	var summary InventorySummary
	digest := sha256.New()
	for _, key := range keys {
		if err := appendInventoryKey(ctx, &summary, digest, key); err != nil {
			return InventorySummary{}, err
		}
	}
	copy(summary.Digest[:], digest.Sum(nil))
	return summary, nil
}

func appendInventoryKey(ctx context.Context, summary *InventorySummary, digest hash.Hash, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(key) == 0 {
		return ErrInventoryKeyEmpty
	}
	if uint64(len(key)) > uint64(^uint32(0)) {
		return fmt.Errorf("storage: inventory key too large: %d", len(key))
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(key)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(key)
	summary.KeyCount++
	summary.KeyBytes += int64(len(key))
	return nil
}
