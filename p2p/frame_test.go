package p2p

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReadFrame_roundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello-world")
	require.NoError(t, WriteFrame(&buf, payload, 1024))
	got, err := ReadFrame(bufio.NewReader(&buf), 1024)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestReadFrame_badMagic(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("XXXX\x00\x00\x00\x05hello")))
	_, err := ReadFrame(r, 1024)
	assert.ErrorIs(t, err, ErrBadMagic)
}

func TestWriteFrame_tooLarge(t *testing.T) {
	var w bytes.Buffer
	err := WriteFrame(&w, make([]byte, 100), 10)
	assert.ErrorIs(t, err, ErrFrameTooLarge)
}

func TestHandshakeVersionV1(t *testing.T) {
	assert.NotEmpty(t, HandshakeVersionV1)
}

func TestReadFrame_lengthTooLarge(t *testing.T) {
	b := append([]byte{}, FrameMagic...)
	var lenpart [4]byte
	binary.BigEndian.PutUint32(lenpart[:], 9999)
	b = append(b, lenpart[:]...)
	r := bufio.NewReader(bytes.NewReader(b))
	_, err := ReadFrame(r, 100)
	assert.ErrorIs(t, err, ErrFrameTooLarge)
}
