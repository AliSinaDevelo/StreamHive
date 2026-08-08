package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testTransportRecord() Record {
	digest := sha256.Sum256([]byte("streamhive-lifecycle-blob"))
	return Record{
		Namespace:   []byte("documents"),
		LogicalKey:  []byte("readme.md"),
		State:       StatePresent,
		BlobKey:     digest[:],
		Version:     Version{Epoch: 4, Sequence: 9},
		AuthorityID: "node-a",
	}
}

func TestRecordTransportRoundTrip(t *testing.T) {
	want := testTransportRecord()
	payload, err := EncodeRecord(want, TransportLimits{})
	require.NoError(t, err)

	got, err := DecodeRecord(payload, TransportLimits{})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, LifecycleRecordMessageType, mustEnvelopeType(t, payload))
}

func TestRecordTransportUsesIndependentPayloadAndRecordLimits(t *testing.T) {
	record := testTransportRecord()
	_, err := EncodeRecord(record, TransportLimits{RecordLimits: Limits{MaxLogicalKeyBytes: 2}})
	assert.ErrorIs(t, err, ErrLogicalKeyTooLarge)

	encoded, err := EncodeRecord(record, TransportLimits{MaxPayloadBytes: 32})
	require.ErrorIs(t, err, ErrLifecyclePayloadTooLarge)
	assert.Nil(t, encoded)
}

func TestDecodeRecordRejectsOversizedPayloadBeforeDecode(t *testing.T) {
	limits := TransportLimits{MaxPayloadBytes: 32}
	payload := bytes.Repeat([]byte{'x'}, limits.MaxPayloadBytes+1)

	_, err := DecodeRecord(payload, limits)
	assert.ErrorIs(t, err, ErrLifecyclePayloadTooLarge)
}

func TestDecodeRecordRejectsUnknownMessageType(t *testing.T) {
	payload := []byte(`{"type":"blob.put","record":{}}`)

	_, err := DecodeRecord(payload, TransportLimits{})
	assert.ErrorIs(t, err, ErrLifecycleMessageType)
}

func TestDecodeRecordRejectsMalformedAndTrailingData(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    error
	}{
		{name: "malformed json", payload: []byte(`{"type":`), want: ErrLifecycleEnvelopeMalformed},
		{
			name:    "unknown envelope field",
			payload: append(bytes.TrimSuffix(validTransportPayload(t), []byte("}")), []byte(`,"extra":true}`)...),
			want:    ErrLifecycleEnvelopeMalformed,
		},
		{
			name:    "second json value",
			payload: append(validTransportPayload(t), []byte(` {}`)...),
			want:    ErrLifecycleEnvelopeTrailingData,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRecord(tt.payload, TransportLimits{})
			require.Error(t, err)
			assert.True(t, errors.Is(err, tt.want), "err=%v", err)
		})
	}
}

func TestDecodeRecordRejectsInvalidInnerRecord(t *testing.T) {
	payload := []byte(`{"type":"lifecycle.record","record":{"namespace":"","logical_key":"","state":"deleted","version":{"epoch":1,"sequence":1},"authority_id":"node-a"}}`)

	_, err := DecodeRecord(payload, TransportLimits{})
	assert.ErrorIs(t, err, ErrLifecycleEnvelopeMalformed)
	assert.ErrorIs(t, err, ErrNamespaceEmpty)
}

func validTransportPayload(t *testing.T) []byte {
	t.Helper()
	payload, err := EncodeRecord(testTransportRecord(), TransportLimits{})
	require.NoError(t, err)
	return payload
}

func mustEnvelopeType(t *testing.T, payload []byte) string {
	t.Helper()
	var envelope RecordEnvelope
	require.NoError(t, json.Unmarshal(payload, &envelope))
	return envelope.Type
}
