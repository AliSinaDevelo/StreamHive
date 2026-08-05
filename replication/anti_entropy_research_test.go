package replication

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// These helpers model candidate inventory envelopes without adding a wire type.
// They keep the anti-entropy decision measurable until a protocol change is justified.
type researchRangeDigest struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Root  string `json:"root"`
}

type researchDigestEnvelope struct {
	Type   string                `json:"type"`
	Count  int                   `json:"count"`
	Root   string                `json:"root,omitempty"`
	Ranges []researchRangeDigest `json:"ranges,omitempty"`
}

func researchInventoryKeys(count, width int) [][]byte {
	keys := make([][]byte, count)
	for i := range keys {
		key := make([]byte, width)
		binary.BigEndian.PutUint64(key[width-8:], uint64(i))
		for j := 0; j < width-8; j++ {
			key[j] = byte(j)
		}
		keys[i] = key
	}
	return keys
}

func researchSortedKeys(keys [][]byte) [][]byte {
	ordered := make([][]byte, len(keys))
	for i, key := range keys {
		ordered[i] = append([]byte(nil), key...)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return string(ordered[i]) < string(ordered[j])
	})
	return ordered
}

func researchRollingDigest(keys [][]byte) [32]byte {
	hash := sha256.New()
	var length [4]byte
	for _, key := range keys {
		binary.BigEndian.PutUint32(length[:], uint32(len(key)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(key)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func researchMerkleRoot(keys [][]byte) [32]byte {
	leaves := make([][32]byte, len(keys))
	for i, key := range keys {
		hash := sha256.New()
		_, _ = hash.Write([]byte("leaf"))
		_, _ = hash.Write(key)
		copy(leaves[i][:], hash.Sum(nil))
	}
	for len(leaves) > 1 {
		next := make([][32]byte, 0, (len(leaves)+1)/2)
		for i := 0; i < len(leaves); i += 2 {
			hash := sha256.New()
			_, _ = hash.Write([]byte("node"))
			_, _ = hash.Write(leaves[i][:])
			if i+1 < len(leaves) {
				_, _ = hash.Write(leaves[i+1][:])
			} else {
				_, _ = hash.Write(leaves[i][:])
			}
			var digest [32]byte
			copy(digest[:], hash.Sum(nil))
			next = append(next, digest)
		}
		leaves = next
	}
	if len(leaves) == 0 {
		return [32]byte{}
	}
	return leaves[0]
}

func researchRangeEnvelope(keys [][]byte, width int) []byte {
	ranges := make([]researchRangeDigest, 0, (len(keys)+width-1)/width)
	for start := 0; start < len(keys); start += width {
		end := start + width
		if end > len(keys) {
			end = len(keys)
		}
		digest := researchMerkleRoot(keys[start:end])
		ranges = append(ranges, researchRangeDigest{
			Start: start,
			End:   end,
			Root:  hex.EncodeToString(digest[:]),
		})
	}
	payload, _ := json.Marshal(researchDigestEnvelope{
		Type:   "research.merkle-ranges",
		Count:  len(keys),
		Ranges: ranges,
	})
	return payload
}

func researchRollingEnvelope(keys [][]byte) []byte {
	digest := researchRollingDigest(keys)
	payload, _ := json.Marshal(researchDigestEnvelope{
		Type:  "research.digest",
		Count: len(keys),
		Root:  hex.EncodeToString(digest[:]),
	})
	return payload
}

func TestResearchInventoryEnvelopeSizes(t *testing.T) {
	for _, width := range []int{32, 512} {
		for _, count := range []int{128, 1024, 4096} {
			keys := researchSortedKeys(researchInventoryKeys(count, width))
			flat, err := EncodeBlobHas(keys, Limits{MaxKeyBytes: width, MaxKeys: count})
			requireNoResearchError(t, err)
			rolling := researchRollingEnvelope(keys)
			ranges := researchRangeEnvelope(keys, 256)
			t.Logf("key_bytes=%d keys=%d flat_json=%dB rolling_root=%dB range_digests=%dB", width, count, len(flat), len(rolling), len(ranges))
		}
	}
}

func BenchmarkResearchInventory(b *testing.B) {
	for _, width := range []int{32, 512} {
		for _, count := range []int{128, 1024, 4096} {
			keys := researchSortedKeys(researchInventoryKeys(count, width))
			limits := Limits{MaxKeyBytes: width, MaxKeys: count}
			b.Run(fmt.Sprintf("flat_encode/key_bytes=%d/keys=%d", width, count), func(b *testing.B) {
				var wireBytes int
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					payload, err := EncodeBlobHas(keys, limits)
					if err != nil {
						b.Fatal(err)
					}
					wireBytes = len(payload)
				}
				b.ReportMetric(float64(wireBytes), "wire_bytes/op")
			})

			b.Run(fmt.Sprintf("rolling_root/key_bytes=%d/keys=%d", width, count), func(b *testing.B) {
				var wireBytes int
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					wireBytes = len(researchRollingEnvelope(keys))
				}
				b.ReportMetric(float64(wireBytes), "wire_bytes/op")
			})

			b.Run(fmt.Sprintf("range_digests/key_bytes=%d/keys=%d", width, count), func(b *testing.B) {
				var wireBytes int
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					wireBytes = len(researchRangeEnvelope(keys, 256))
				}
				b.ReportMetric(float64(wireBytes), "wire_bytes/op")
			})
		}
	}
}

func requireNoResearchError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
