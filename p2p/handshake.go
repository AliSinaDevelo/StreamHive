package p2p

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"
)

// HandshakeVersionV1 is the initial handshake string carried in the first application frame.
const HandshakeVersionV1 = "streamhive/1"

const (
	handshakeTypePeerAuth       = "peer.auth"
	handshakeTypePeerAuthOK     = "peer.auth.ok"
	handshakeTypePeerAuthReject = "peer.auth.reject"
)

// DefaultPeerAuthTimeout bounds optional application-level peer auth handshakes.
const DefaultPeerAuthTimeout = 5 * time.Second

var (
	// ErrPeerAuthFailed is returned when the peer auth handshake is malformed or incomplete.
	ErrPeerAuthFailed = errors.New("p2p: peer auth failed")
	// ErrPeerAuthRejected is returned when a peer presents an invalid auth token.
	ErrPeerAuthRejected = errors.New("p2p: peer auth rejected")
)

type peerAuthMessage struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	Token   string `json:"token,omitempty"`
	Error   string `json:"error,omitempty"`
}

func encodePeerAuth(token string) ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:    handshakeTypePeerAuth,
		Version: HandshakeVersionV1,
		Token:   token,
	})
}

func encodePeerAuthOK() ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:    handshakeTypePeerAuthOK,
		Version: HandshakeVersionV1,
	})
}

func encodePeerAuthReject() ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:    handshakeTypePeerAuthReject,
		Version: HandshakeVersionV1,
		Error:   "unauthorized",
	})
}

func validatePeerAuthPayload(payload []byte, token string) error {
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Type != handshakeTypePeerAuth || msg.Version != HandshakeVersionV1 {
		return ErrPeerAuthFailed
	}
	if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(token)) != 1 {
		return ErrPeerAuthRejected
	}
	return nil
}

func validatePeerAuthAck(payload []byte) error {
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Version != HandshakeVersionV1 {
		return ErrPeerAuthFailed
	}
	switch msg.Type {
	case handshakeTypePeerAuthOK:
		return nil
	case handshakeTypePeerAuthReject:
		return ErrPeerAuthRejected
	default:
		return ErrPeerAuthFailed
	}
}
