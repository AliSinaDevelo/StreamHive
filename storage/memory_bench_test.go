package storage

import (
	"context"
	"fmt"
	"strconv"
	"testing"
)

func BenchmarkMemoryStorePut1KB(b *testing.B) {
	ctx := context.Background()
	store := NewMemoryStore()
	value := make([]byte, 1024)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key := []byte(strconv.Itoa(i))
		if err := store.Put(ctx, key, value); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStoreGet1KB(b *testing.B) {
	ctx := context.Background()
	store := NewMemoryStore()
	value := make([]byte, 1024)
	keys := make([][]byte, 1024)
	for i := range keys {
		keys[i] = []byte(strconv.Itoa(i))
		if err := store.Put(ctx, keys[i], value); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := store.Get(ctx, keys[i%len(keys)]); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkMemoryStore4096(b *testing.B) *MemoryStore {
	b.Helper()
	store := NewMemoryStore()
	ctx := context.Background()
	for i := 0; i < 4096; i++ {
		key := []byte(fmt.Sprintf("key-%04d", i))
		if err := store.Put(ctx, key, nil); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

func BenchmarkMemoryStoreListKeys4096(b *testing.B) {
	store := benchmarkMemoryStore4096(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListKeys(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStoreListKeyPages4096(b *testing.B) {
	store := benchmarkMemoryStore4096(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cursor []byte
		for {
			_, next, err := store.ListKeyPage(ctx, cursor, 256)
			if err != nil {
				b.Fatal(err)
			}
			if len(next) == 0 {
				break
			}
			cursor = next
		}
	}
}
