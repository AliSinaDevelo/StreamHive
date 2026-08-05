package storage

import (
	"bytes"
	"sort"
)

// DefaultKeyPageSize is used when a pager receives a non-positive limit.
const DefaultKeyPageSize = 256

func normalizeKeyPageLimit(limit int) int {
	if limit <= 0 {
		return DefaultKeyPageSize
	}
	return limit
}

// insertKeyPage keeps the smallest keys after after in sorted order. It retains
// at most limit entries, making a page scan bounded even when the store is large.
func insertKeyPage(page [][]byte, key, after []byte, limit int) [][]byte {
	if len(after) > 0 && bytes.Compare(key, after) <= 0 {
		return page
	}
	index := sort.Search(len(page), func(i int) bool {
		return bytes.Compare(page[i], key) >= 0
	})
	if index < len(page) && bytes.Equal(page[index], key) {
		return page
	}
	if len(page) >= limit && index == len(page) {
		return page
	}
	page = append(page, nil)
	copy(page[index+1:], page[index:])
	page[index] = append([]byte(nil), key...)
	if len(page) > limit {
		page = page[:limit]
	}
	return page
}
