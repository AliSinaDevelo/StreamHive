package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AliSinaDevelo/StreamHive/storage"
)

const (
	// MessageTypeBlobPut stores or replaces one blob on a receiving peer.
	MessageTypeBlobPut = "blob.put"
	// MessageTypeBlobHas advertises keys available on the sending peer.
	MessageTypeBlobHas = "blob.has"
	// MessageTypeBlobGet requests one blob by key from a peer.
	MessageTypeBlobGet = "blob.get"
	// MessageTypeBlobMissing reports keys missing from the sending peer.
	MessageTypeBlobMissing = "blob.missing"
	// MessageTypeBlobAck acknowledges that a blob key was accepted by a peer.
	MessageTypeBlobAck = "blob.ack"

	// DefaultMaxKeyBytes bounds encoded blob keys.
	DefaultMaxKeyBytes = 512
	// DefaultMaxKeys bounds inventory messages such as blob.has and blob.missing.
	DefaultMaxKeys = 4096
	// DefaultMaxDataBytes bounds replicated blob payloads.
	DefaultMaxDataBytes = 4 << 20
	// DefaultMaxRepairBytes bounds aggregate blob data sent for one anti-entropy request.
	DefaultMaxRepairBytes = 64 << 20
)

var (
	// ErrNilStore is returned when applying a message without a store.
	ErrNilStore = errors.New("replication: nil store")
	// ErrUnknownMessageType is returned for unsupported message types.
	ErrUnknownMessageType = errors.New("replication: unknown message type")
	// ErrKeyEmpty is returned when a blob message has no key.
	ErrKeyEmpty = errors.New("replication: empty key")
	// ErrKeyTooLarge is returned when a key exceeds the configured limit.
	ErrKeyTooLarge = errors.New("replication: key too large")
	// ErrDataTooLarge is returned when a blob exceeds the configured limit.
	ErrDataTooLarge = errors.New("replication: data too large")
	// ErrKeysEmpty is returned when an inventory message has no keys.
	ErrKeysEmpty = errors.New("replication: empty keys")
	// ErrTooManyKeys is returned when an inventory message exceeds the configured limit.
	ErrTooManyKeys = errors.New("replication: too many keys")
	// ErrMessageNotApplicable is returned when Apply receives a non-mutating protocol message.
	ErrMessageNotApplicable = errors.New("replication: message is not directly applicable")
)

// Limits bounds decoded message sizes before they are applied.
type Limits struct {
	MaxKeyBytes    int
	MaxKeys        int
	MaxDataBytes   int
	MaxRepairBytes int
}

// Message is the JSON payload carried inside an SHV1 frame.
type Message struct {
	Type string   `json:"type"`
	Key  []byte   `json:"key,omitempty"`
	Keys [][]byte `json:"keys,omitempty"`
	Data []byte   `json:"data,omitempty"`
}

// EncodeBlobPut returns a frame payload that stores data under key on receivers.
func EncodeBlobPut(key, data []byte, limits Limits) ([]byte, error) {
	msg := Message{
		Type: MessageTypeBlobPut,
		Key:  append([]byte(nil), key...),
		Data: append([]byte(nil), data...),
	}
	if err := validateBlobPut(msg, normalizeLimits(limits)); err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

// EncodeBlobHas returns a frame payload that advertises available blob keys.
func EncodeBlobHas(keys [][]byte, limits Limits) ([]byte, error) {
	return encodeKeyListMessage(MessageTypeBlobHas, keys, limits)
}

// EncodeBlobGet returns a frame payload that requests one blob by key.
func EncodeBlobGet(key []byte, limits Limits) ([]byte, error) {
	msg := Message{
		Type: MessageTypeBlobGet,
		Key:  append([]byte(nil), key...),
	}
	if err := validateKey(msg.Key, normalizeLimits(limits)); err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

// EncodeBlobMissing returns a frame payload that reports missing blob keys.
func EncodeBlobMissing(keys [][]byte, limits Limits) ([]byte, error) {
	return encodeKeyListMessage(MessageTypeBlobMissing, keys, limits)
}

// EncodeBlobAck returns a frame payload that acknowledges one accepted blob key.
func EncodeBlobAck(key []byte, limits Limits) ([]byte, error) {
	msg := Message{
		Type: MessageTypeBlobAck,
		Key:  append([]byte(nil), key...),
	}
	if err := validateKey(msg.Key, normalizeLimits(limits)); err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

// Decode parses a replication payload and validates its declared message type.
func Decode(payload []byte, limits Limits) (Message, error) {
	var msg Message
	if err := json.Unmarshal(payload, &msg); err != nil {
		return Message{}, fmt.Errorf("replication: decode: %w", err)
	}

	normalized := normalizeLimits(limits)
	switch msg.Type {
	case MessageTypeBlobPut:
		if err := validateBlobPut(msg, normalized); err != nil {
			return Message{}, err
		}
	case MessageTypeBlobGet, MessageTypeBlobAck:
		if err := validateKey(msg.Key, normalized); err != nil {
			return Message{}, err
		}
	case MessageTypeBlobHas, MessageTypeBlobMissing:
		if err := validateKeys(msg.Keys, normalized); err != nil {
			return Message{}, err
		}
	default:
		return Message{}, ErrUnknownMessageType
	}

	msg.Key = append([]byte(nil), msg.Key...)
	msg.Keys = cloneKeys(msg.Keys)
	msg.Data = append([]byte(nil), msg.Data...)
	return msg, nil
}

// Apply decodes a replication payload and applies it to store.
func Apply(ctx context.Context, store storage.BlobStore, payload []byte, limits Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil {
		return ErrNilStore
	}

	msg, err := Decode(payload, limits)
	if err != nil {
		return err
	}

	switch msg.Type {
	case MessageTypeBlobPut:
		if err := verifyContentKey(msg.Key, msg.Data); err != nil {
			return err
		}
		return store.Put(ctx, msg.Key, msg.Data)
	default:
		return ErrMessageNotApplicable
	}
}

func verifyContentKey(key, data []byte) error {
	if err := storage.ValidateSHA256Key(key); err != nil {
		return nil
	}
	return storage.VerifySHA256Key(key, data)
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxKeyBytes <= 0 {
		limits.MaxKeyBytes = DefaultMaxKeyBytes
	}
	if limits.MaxKeys <= 0 {
		limits.MaxKeys = DefaultMaxKeys
	}
	if limits.MaxDataBytes <= 0 {
		limits.MaxDataBytes = DefaultMaxDataBytes
	}
	if limits.MaxRepairBytes <= 0 {
		limits.MaxRepairBytes = DefaultMaxRepairBytes
	}
	return limits
}

func validateBlobPut(msg Message, limits Limits) error {
	if err := validateKey(msg.Key, limits); err != nil {
		return err
	}
	if len(msg.Data) > limits.MaxDataBytes {
		return ErrDataTooLarge
	}
	return nil
}

func encodeKeyListMessage(messageType string, keys [][]byte, limits Limits) ([]byte, error) {
	msg := Message{
		Type: messageType,
		Keys: cloneKeys(keys),
	}
	if err := validateKeys(msg.Keys, normalizeLimits(limits)); err != nil {
		return nil, err
	}
	return json.Marshal(msg)
}

func validateKey(key []byte, limits Limits) error {
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	if len(key) > limits.MaxKeyBytes {
		return ErrKeyTooLarge
	}
	return nil
}

func validateKeys(keys [][]byte, limits Limits) error {
	if len(keys) == 0 {
		return ErrKeysEmpty
	}
	if len(keys) > limits.MaxKeys {
		return ErrTooManyKeys
	}
	for _, key := range keys {
		if err := validateKey(key, limits); err != nil {
			return err
		}
	}
	return nil
}

func cloneKeys(keys [][]byte) [][]byte {
	if keys == nil {
		return nil
	}
	out := make([][]byte, len(keys))
	for i, key := range keys {
		out[i] = append([]byte(nil), key...)
	}
	return out
}
