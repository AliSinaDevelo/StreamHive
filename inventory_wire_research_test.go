package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/require"
)

type inventoryResearchPeer struct {
	testPeer
	frames    int
	wireBytes int
	payloads  [][]byte
}

func (p *inventoryResearchPeer) WriteFrame(payload []byte, _ int) error {
	p.frames++
	p.wireBytes += len(payload)
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
	return nil
}

func researchInventoryStore(b *testing.B, count, width int) *storage.MemoryStore {
	b.Helper()
	store := storage.NewMemoryStore()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		key := make([]byte, width)
		binary.BigEndian.PutUint64(key[width-8:], uint64(i))
		for j := 0; j < width-8; j++ {
			key[j] = byte(j)
		}
		if err := store.Put(ctx, key, nil); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

func decodeAndProbeInventory(ctx context.Context, payloads [][]byte, store storage.BlobStore, limits replication.Limits) (int, int, error) {
	probes := 0
	missing := 0
	for _, payload := range payloads {
		msg, err := replication.Decode(payload, limits)
		if err != nil {
			return probes, missing, err
		}
		keys, err := missingKeysFromStore(ctx, store, msg.Keys)
		if err != nil {
			return probes, missing, err
		}
		probes += len(msg.Keys)
		missing += len(keys)
	}
	return probes, missing, nil
}

func TestResearchInventoryWireFrameBudget(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	limits := replication.Limits{MaxKeyBytes: 512, MaxKeys: replication.DefaultMaxKeys}
	for i := 0; i < replication.DefaultMaxKeys; i++ {
		key := make([]byte, 512)
		binary.BigEndian.PutUint64(key[504:], uint64(i))
		require.NoError(t, store.Put(ctx, key, nil))
	}

	peer := &inventoryResearchPeer{}
	require.NoError(t, sendBlobHas(ctx, peer, store, limits, 0, &replicationMetrics{}))
	probes, missing, err := decodeAndProbeInventory(ctx, peer.payloads, storage.NewMemoryStore(), limits)
	require.NoError(t, err)
	require.Equal(t, replication.DefaultMaxKeys, probes)
	require.Equal(t, replication.DefaultMaxKeys, missing)
	require.Len(t, peer.payloads, 1)
	require.Less(t, peer.wireBytes, 4<<20)
}

func BenchmarkResearchInventoryExchange(b *testing.B) {
	for _, width := range []int{32, 64, 512} {
		for _, count := range []int{4096, 65536} {
			b.Run(fmt.Sprintf("key_bytes=%d/keys=%d", width, count), func(b *testing.B) {
				store := researchInventoryStore(b, count, width)
				limits := replication.Limits{MaxKeyBytes: width, MaxKeys: replication.DefaultMaxKeys}
				ctx := context.Background()
				var frames, wireBytes, probes, missing int
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					peer := &inventoryResearchPeer{}
					if err := sendBlobHas(ctx, peer, store, limits, 0, &replicationMetrics{}); err != nil {
						b.Fatal(err)
					}
					probeStore := &hasProbeStore{BlobStore: storage.NewMemoryStore()}
					var err error
					probes, missing, err = decodeAndProbeInventory(ctx, peer.payloads, probeStore, limits)
					if err != nil {
						b.Fatal(err)
					}
					frames = peer.frames
					wireBytes = peer.wireBytes
				}
				b.ReportMetric(float64(frames), "frames/op")
				b.ReportMetric(float64(wireBytes), "wire_bytes/op")
				b.ReportMetric(float64(probes), "key_probes/op")
				b.ReportMetric(float64(missing), "missing_keys/op")
			})
		}
	}
}
