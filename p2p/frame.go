package p2p

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// Wire format: Magic (4) + length (4, big-endian) + payload.
var (
	// FrameMagic identifies StreamHive frames on the wire.
	FrameMagic = []byte("SHV1")

	// DefaultMaxFrameBytes caps a single frame payload (DoS bound).
	DefaultMaxFrameBytes = 4 << 20
)

var (
	// ErrBadMagic means the stream does not start with FrameMagic.
	ErrBadMagic = errors.New("p2p: bad frame magic")
	// ErrFrameTooLarge means the declared length exceeds the configured maximum.
	ErrFrameTooLarge = errors.New("p2p: frame too large")
)

// WriteFrame writes one length-prefixed frame. Payload must be <= maxPayload.
func WriteFrame(w io.Writer, payload []byte, maxPayload int) error {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFrameBytes
	}
	if len(payload) > maxPayload {
		return ErrFrameTooLarge
	}
	var hdr [8]byte
	copy(hdr[0:4], FrameMagic)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame using r. maxPayload bounds the declared length.
func ReadFrame(r *bufio.Reader, maxPayload int) ([]byte, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFrameBytes
	}
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(magic[:], FrameMagic) {
		return nil, ErrBadMagic
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if int(n) > maxPayload {
		return nil, ErrFrameTooLarge
	}
	if n == 0 {
		return nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
