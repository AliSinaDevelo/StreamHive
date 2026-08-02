package replication

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeBlobPut(t *testing.T) {
	payload, err := EncodeBlobPut([]byte("alpha"), []byte("hello"), Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, MessageTypeBlobPut, msg.Type)
	assert.Equal(t, []byte("alpha"), msg.Key)
	assert.Equal(t, []byte("hello"), msg.Data)
}

func TestApplyBlobPut(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	payload, err := EncodeBlobPut([]byte("k1"), []byte("value"), Limits{})
	require.NoError(t, err)

	require.NoError(t, Apply(ctx, store, payload, Limits{}))
	got, err := store.Get(ctx, []byte("k1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), got)
}

func TestEncodeDecodeBlobGet(t *testing.T) {
	payload, err := EncodeBlobGet([]byte("alpha"), Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, MessageTypeBlobGet, msg.Type)
	assert.Equal(t, []byte("alpha"), msg.Key)
}

func TestEncodeDecodeBlobHas(t *testing.T) {
	keys := [][]byte{[]byte("alpha"), []byte("beta")}
	payload, err := EncodeBlobHas(keys, Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, MessageTypeBlobHas, msg.Type)
	assert.Equal(t, [][]byte{[]byte("alpha"), []byte("beta")}, msg.Keys)
}

func TestEncodeDecodeBlobMissing(t *testing.T) {
	keys := [][]byte{[]byte("alpha"), []byte("beta")}
	payload, err := EncodeBlobMissing(keys, Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, MessageTypeBlobMissing, msg.Type)
	assert.Equal(t, [][]byte{[]byte("alpha"), []byte("beta")}, msg.Keys)
}

func TestEncodeDecodeBlobAck(t *testing.T) {
	payload, err := EncodeBlobAck([]byte("alpha"), Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, MessageTypeBlobAck, msg.Type)
	assert.Equal(t, []byte("alpha"), msg.Key)
}

func TestApplyRejectsNilStore(t *testing.T) {
	payload, err := EncodeBlobPut([]byte("k1"), []byte("value"), Limits{})
	require.NoError(t, err)

	err = Apply(context.Background(), nil, payload, Limits{})
	assert.ErrorIs(t, err, ErrNilStore)
}

func TestApplyRespectsContextCancellation(t *testing.T) {
	payload, err := EncodeBlobPut([]byte("k1"), []byte("value"), Limits{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = Apply(ctx, storage.NewMemoryStore(), payload, Limits{})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestApplyRejectsInventoryMessages(t *testing.T) {
	payload, err := EncodeBlobGet([]byte("k1"), Limits{})
	require.NoError(t, err)

	err = Apply(context.Background(), storage.NewMemoryStore(), payload, Limits{})
	assert.ErrorIs(t, err, ErrMessageNotApplicable)
}

func TestDecodeRejectsUnknownMessageType(t *testing.T) {
	payload, err := json.Marshal(Message{Type: "peer.hello"})
	require.NoError(t, err)

	_, err = Decode(payload, Limits{})
	assert.ErrorIs(t, err, ErrUnknownMessageType)
}

func TestKeyListValidation(t *testing.T) {
	tests := []struct {
		name   string
		keys   [][]byte
		limits Limits
		want   error
	}{
		{
			name: "empty keys",
			keys: nil,
			want: ErrKeysEmpty,
		},
		{
			name: "too many keys",
			keys: [][]byte{[]byte("a"), []byte("b")},
			limits: Limits{
				MaxKeys: 1,
			},
			want: ErrTooManyKeys,
		},
		{
			name: "empty key",
			keys: [][]byte{[]byte("a"), nil},
			want: ErrKeyEmpty,
		},
		{
			name: "key too large",
			keys: [][]byte{[]byte("abcd")},
			limits: Limits{
				MaxKeyBytes: 3,
			},
			want: ErrKeyTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncodeBlobHas(tt.keys, tt.limits)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestBlobPutValidation(t *testing.T) {
	tests := []struct {
		name   string
		key    []byte
		data   []byte
		limits Limits
		want   error
	}{
		{
			name: "empty key",
			key:  nil,
			want: ErrKeyEmpty,
		},
		{
			name:   "key too large",
			key:    []byte("abcd"),
			limits: Limits{MaxKeyBytes: 3},
			want:   ErrKeyTooLarge,
		},
		{
			name:   "data too large",
			key:    []byte("k"),
			data:   []byte("abcd"),
			limits: Limits{MaxDataBytes: 3},
			want:   ErrDataTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncodeBlobPut(tt.key, tt.data, tt.limits)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestDecodeReturnsCopiedSlices(t *testing.T) {
	payload, err := EncodeBlobPut([]byte("alpha"), []byte("hello"), Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	msg.Key[0] = 'x'
	msg.Data[0] = 'y'

	again, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, []byte("alpha"), again.Key)
	assert.Equal(t, []byte("hello"), again.Data)
}

func TestDecodeReturnsCopiedKeyLists(t *testing.T) {
	payload, err := EncodeBlobHas([][]byte{[]byte("alpha")}, Limits{})
	require.NoError(t, err)

	msg, err := Decode(payload, Limits{})
	require.NoError(t, err)
	msg.Keys[0][0] = 'x'

	again, err := Decode(payload, Limits{})
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("alpha")}, again.Keys)
}

func TestDecodeInvalidJSON(t *testing.T) {
	_, err := Decode([]byte("{"), Limits{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnknownMessageType))
}
