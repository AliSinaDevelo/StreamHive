package storage

import (
	"bytes"
	"context"
	"errors"
	"sort"
)

// IntegritySummary is a live aggregate of the blobs visible through an inventory
// lister. Content-addressed keys are verified against their stored bytes; opaque
// keys are counted without imposing a content-addressing contract on them.
type IntegritySummary struct {
	KeyCount             int
	KeyBytes             int64
	ContentAddressedKeys int
	VerifiedKeys         int
	VerifiedBytes        int64
	OpaqueKeys           int
	OpaqueBytes          int64
	CorruptKeys          int
	MissingKeys          int
}

// InspectInventory reads the current inventory and classifies each visible key.
// Native pagers are consumed one bounded page at a time; older listers use their
// complete-list compatibility path. The scan is live and does not claim a
// snapshot-consistent view while the store is changing.
func InspectInventory(ctx context.Context, store BlobStore, lister BlobKeyLister) (IntegritySummary, error) {
	if store == nil {
		return IntegritySummary{}, errors.New("storage: nil blob store")
	}
	if lister == nil {
		return IntegritySummary{}, errors.New("storage: nil inventory lister")
	}
	if err := ctx.Err(); err != nil {
		return IntegritySummary{}, err
	}

	var summary IntegritySummary
	if pager, ok := lister.(BlobKeyPager); ok {
		return summary, inspectPagedInventory(ctx, store, pager, &summary)
	}
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return IntegritySummary{}, err
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i], keys[j]) < 0
	})
	if err := inspectInventoryKeys(ctx, store, keys, &summary); err != nil {
		return IntegritySummary{}, err
	}
	return summary, nil
}

func inspectPagedInventory(ctx context.Context, store BlobStore, pager BlobKeyPager, summary *IntegritySummary) error {
	var cursor []byte
	hasCursor := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		keys, next, err := pager.ListKeyPage(ctx, cursor, DefaultKeyPageSize)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			if len(next) != 0 {
				return errors.Join(ErrInventoryPagerInvalid, errors.New("storage: empty page returned cursor"))
			}
			return nil
		}
		if len(keys) > DefaultKeyPageSize || len(next) == 0 || !bytes.Equal(next, keys[len(keys)-1]) {
			return ErrInventoryPagerInvalid
		}
		for i, key := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(key) == 0 {
				return ErrInventoryKeyEmpty
			}
			if hasCursor && bytes.Compare(key, cursor) <= 0 {
				return ErrInventoryPagerInvalid
			}
			if i > 0 && bytes.Compare(key, keys[i-1]) <= 0 {
				return ErrInventoryPagerInvalid
			}
		}
		if err := inspectInventoryKeys(ctx, store, keys, summary); err != nil {
			return err
		}
		cursor = append(cursor[:0], next...)
		hasCursor = true
	}
}

func inspectInventoryKeys(ctx context.Context, store BlobStore, keys [][]byte, summary *IntegritySummary) error {
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(key) == 0 {
			return ErrInventoryKeyEmpty
		}
		summary.KeyCount++
		summary.KeyBytes += int64(len(key))
		contentAddressed := len(key) == SHA256KeyBytes
		if contentAddressed {
			summary.ContentAddressedKeys++
		} else {
			summary.OpaqueKeys++
		}

		data, err := store.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			summary.MissingKeys++
			continue
		}
		if err != nil {
			if contentAddressed && errors.Is(err, ErrSHA256Mismatch) {
				summary.CorruptKeys++
				continue
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if contentAddressed {
			if err := VerifySHA256Key(key, data); err != nil {
				if errors.Is(err, ErrSHA256Mismatch) {
					summary.CorruptKeys++
					continue
				}
				return err
			}
			summary.VerifiedKeys++
			summary.VerifiedBytes += int64(len(data))
		} else {
			summary.OpaqueBytes += int64(len(data))
		}
	}
	return nil
}
