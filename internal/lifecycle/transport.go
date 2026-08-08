package lifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// LifecycleRecordMessageType identifies a logical lifecycle mutation envelope.
	LifecycleRecordMessageType = "lifecycle.record"
	// DefaultMaxLifecyclePayloadBytes bounds the JSON envelope independently from raw blobs.
	DefaultMaxLifecyclePayloadBytes = 128 << 10
)

var (
	// ErrLifecyclePayloadTooLarge is returned before decoding an oversized envelope.
	ErrLifecyclePayloadTooLarge = errors.New("lifecycle: transport payload too large")
	// ErrLifecycleEnvelopeMalformed is returned for invalid JSON or invalid record envelopes.
	ErrLifecycleEnvelopeMalformed = errors.New("lifecycle: malformed transport envelope")
	// ErrLifecycleEnvelopeTrailingData is returned when one payload contains more than one JSON value.
	ErrLifecycleEnvelopeTrailingData = errors.New("lifecycle: trailing transport envelope data")
	// ErrLifecycleMessageType is returned when a lifecycle decoder receives another protocol message.
	ErrLifecycleMessageType = errors.New("lifecycle: unexpected transport message type")
)

// TransportLimits bounds a lifecycle record envelope and its inner record separately.
type TransportLimits struct {
	MaxPayloadBytes int
	RecordLimits    Limits
}

func (l TransportLimits) normalized() TransportLimits {
	if l.MaxPayloadBytes <= 0 {
		l.MaxPayloadBytes = DefaultMaxLifecyclePayloadBytes
	}
	l.RecordLimits = l.RecordLimits.normalized()
	return l
}

// RecordEnvelope carries one validated lifecycle mutation over the wire.
type RecordEnvelope struct {
	Type   string `json:"type"`
	Record Record `json:"record"`
}

// EncodeRecord validates and encodes one lifecycle record without applying it.
func EncodeRecord(record Record, limits TransportLimits) ([]byte, error) {
	limits = limits.normalized()
	if err := record.Validate(limits.RecordLimits); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(RecordEnvelope{
		Type:   LifecycleRecordMessageType,
		Record: record,
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: encode transport envelope: %w", err)
	}
	if len(payload) > limits.MaxPayloadBytes {
		return nil, ErrLifecyclePayloadTooLarge
	}
	return payload, nil
}

// DecodeRecord validates one lifecycle record envelope without applying it.
func DecodeRecord(payload []byte, limits TransportLimits) (Record, error) {
	limits = limits.normalized()
	if len(payload) > limits.MaxPayloadBytes {
		return Record{}, ErrLifecyclePayloadTooLarge
	}

	var envelope RecordEnvelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Record{}, errors.Join(ErrLifecycleEnvelopeMalformed, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Record{}, errors.Join(ErrLifecycleEnvelopeMalformed, ErrLifecycleEnvelopeTrailingData)
		}
		return Record{}, errors.Join(ErrLifecycleEnvelopeMalformed, ErrLifecycleEnvelopeTrailingData, err)
	}
	if envelope.Type != LifecycleRecordMessageType {
		return Record{}, ErrLifecycleMessageType
	}
	if err := envelope.Record.Validate(limits.RecordLimits); err != nil {
		return Record{}, errors.Join(ErrLifecycleEnvelopeMalformed, err)
	}
	return envelope.Record, nil
}
