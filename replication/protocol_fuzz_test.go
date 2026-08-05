package replication

import (
	"bytes"
	"testing"
)

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"type":"blob.put","key":"YQ==","data":"Yg=="}`))
	f.Add([]byte(`{"type":"blob.has","keys":["YQ==","Yg=="]}`))
	f.Add([]byte(`{"type":"blob.put","key":"!!!","data":"Yg=="}`))
	f.Add([]byte(`{"type":"peer.hello","key":"YQ=="}`))
	f.Add([]byte(`{"type":"blob.missing","keys":[]}`))
	f.Add([]byte("{"))

	f.Fuzz(func(t *testing.T, payload []byte) {
		// Keep malformed-input smoke tests bounded while still varying every limit.
		if len(payload) > 64<<10 {
			payload = payload[:64<<10]
		}
		limits := Limits{
			MaxKeyBytes:  1 + len(payload)%32,
			MaxKeys:      1 + len(payload)%16,
			MaxDataBytes: 1 + len(payload)%1024,
		}
		_, _ = Decode(payload, limits)
	})
}

func FuzzEncodeDecodeBlobPut(f *testing.F) {
	f.Add([]byte("alpha"), []byte("hello"))
	f.Add([]byte{0}, []byte{})
	f.Add([]byte("key"), []byte{0, 1, 2, 3})

	f.Fuzz(func(t *testing.T, key, data []byte) {
		if len(key) > 128 {
			key = key[:128]
		}
		if len(data) > 4096 {
			data = data[:4096]
		}
		if len(key) == 0 {
			key = []byte{0}
		}

		limits := Limits{MaxKeyBytes: 128, MaxDataBytes: 4096}
		payload, err := EncodeBlobPut(key, data, limits)
		if err != nil {
			return
		}
		got, err := Decode(payload, limits)
		if err != nil {
			t.Fatalf("round-trip decode: %v", err)
		}
		if !bytes.Equal(got.Key, key) {
			t.Fatalf("key changed during round-trip: got %x want %x", got.Key, key)
		}
		if !bytes.Equal(got.Data, data) {
			t.Fatalf("data changed during round-trip: got %x want %x", got.Data, data)
		}
	})
}
