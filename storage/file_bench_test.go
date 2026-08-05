package storage

import (
	"context"
	"fmt"
	"testing"
)

func benchmarkFileStoreDir(b *testing.B, count int) string {
	b.Helper()
	dir := b.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < count; i++ {
		key := []byte(fmt.Sprintf("key-%05d", i))
		if err := store.Put(ctx, key, nil); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

func benchmarkFileStore(b *testing.B, count int) *FileStore {
	b.Helper()
	dir := benchmarkFileStoreDir(b, count)
	store, err := NewFileStore(dir)
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := store.ListKeyPage(context.Background(), nil, 1); err != nil {
		b.Fatal(err)
	}
	return store
}

func benchmarkFileStoreListPages(b *testing.B, store *FileStore) {
	b.Helper()
	ctx := context.Background()
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

func BenchmarkFileStoreListKeys4096(b *testing.B) {
	store := benchmarkFileStore(b, 4096)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListKeys(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileStoreListKeyPages4096(b *testing.B) {
	store := benchmarkFileStore(b, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	benchmarkFileStoreListPages(b, store)
}

func BenchmarkFileStoreListKeys65536(b *testing.B) {
	store := benchmarkFileStore(b, 65536)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListKeys(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileStoreListKeyPages65536(b *testing.B) {
	store := benchmarkFileStore(b, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	benchmarkFileStoreListPages(b, store)
}

func BenchmarkFileStoreBuildIndex4096(b *testing.B) {
	dir := benchmarkFileStoreDir(b, 4096)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store, err := NewFileStore(dir)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.ListKeyPage(ctx, nil, 256); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileStoreBuildIndex65536(b *testing.B) {
	dir := benchmarkFileStoreDir(b, 65536)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store, err := NewFileStore(dir)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.ListKeyPage(ctx, nil, 256); err != nil {
			b.Fatal(err)
		}
	}
}
