package p2p

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte(""), 1)
	f.Add([]byte("XXXX\x00\x00\x00\x05hello"), 1024)
	f.Add(frameSeed([]byte("hello")), 1024)
	f.Add(frameLengthSeed(^uint32(0)), 64)

	f.Fuzz(func(t *testing.T, wire []byte, maxPayload int) {
		maxPayload = 1 + int(uint(maxPayload)%4096)
		payload, err := ReadFrame(bufio.NewReader(bytes.NewReader(wire)), maxPayload)
		if err == nil && len(payload) > maxPayload {
			t.Fatalf("read payload exceeds bound: got %d max %d", len(payload), maxPayload)
		}
	})
}

func FuzzWriteReadFrame(f *testing.F) {
	f.Add([]byte("hello"), 1024)
	f.Add([]byte{}, 1)
	f.Add([]byte{0, 1, 2, 3}, 4)

	f.Fuzz(func(t *testing.T, payload []byte, maxPayload int) {
		maxPayload = 1 + int(uint(maxPayload)%4096)
		if len(payload) > maxPayload {
			payload = payload[:maxPayload]
		}

		var wire bytes.Buffer
		if err := WriteFrame(&wire, payload, maxPayload); err != nil {
			t.Fatalf("write frame: %v", err)
		}
		got, err := ReadFrame(bufio.NewReader(&wire), maxPayload)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("frame changed during round-trip: got %x want %x", got, payload)
		}
	})
}

func frameSeed(payload []byte) []byte {
	var wire bytes.Buffer
	_ = WriteFrame(&wire, payload, DefaultMaxFrameBytes)
	return wire.Bytes()
}

func frameLengthSeed(length uint32) []byte {
	wire := append([]byte(nil), FrameMagic...)
	var lengthBytes [4]byte
	binary.BigEndian.PutUint32(lengthBytes[:], length)
	return append(wire, lengthBytes[:]...)
}
