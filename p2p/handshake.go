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

// MaxPeerAuthIdentityBytes bounds the application identity carried in auth frames.
const MaxPeerAuthIdentityBytes = 128

var (
	// ErrPeerAuthFailed is returned when the peer auth handshake is malformed or incomplete.
	ErrPeerAuthFailed = errors.New("p2p: peer auth failed")
	// ErrPeerAuthRejected is returned when a peer presents an invalid auth token.
	ErrPeerAuthRejected = errors.New("p2p: peer auth rejected")
	// ErrPeerAuthIdentityInvalid is returned when an application identity is malformed.
	ErrPeerAuthIdentityInvalid = errors.New("p2p: peer auth identity invalid")
)

type peerAuthMessage struct {
	Type     string `json:"type"`
	Version  string `json:"version"`
	Token    string `json:"token,omitempty"`
	Identity string `json:"identity,omitempty"`
	Error    string `json:"error,omitempty"`
}

func encodePeerAuth(token, identity string) ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:     handshakeTypePeerAuth,
		Version:  HandshakeVersionV1,
		Token:    token,
		Identity: identity,
	})
}

func encodePeerAuthOK(identity string) ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:     handshakeTypePeerAuthOK,
		Version:  HandshakeVersionV1,
		Identity: identity,
	})
}

func encodePeerAuthReject() ([]byte, error) {
	return json.Marshal(peerAuthMessage{
		Type:    handshakeTypePeerAuthReject,
		Version: HandshakeVersionV1,
		Error:   "unauthorized",
	})
}

func validatePeerAuthPayload(payload []byte, token string) (string, error) {
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Type != handshakeTypePeerAuth || msg.Version != HandshakeVersionV1 {
		return "", ErrPeerAuthFailed
	}
	if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(token)) != 1 {
		return "", ErrPeerAuthRejected
	}
	if err := validatePeerIdentity(msg.Identity); err != nil {
		return "", err
	}
	return msg.Identity, nil
}

func validatePeerAuthAck(payload []byte) (string, error) {
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Version != HandshakeVersionV1 {
		return "", ErrPeerAuthFailed
	}
	switch msg.Type {
	case handshakeTypePeerAuthOK:
		if err := validatePeerIdentity(msg.Identity); err != nil {
			return "", err
		}
		return msg.Identity, nil
	case handshakeTypePeerAuthReject:
		return "", ErrPeerAuthRejected
	default:
		return "", ErrPeerAuthFailed
	}
}

func validatePeerIdentity(identity string) error {
	if identity == "" {
		return nil
	}
	if len(identity) > MaxPeerAuthIdentityBytes {
		return ErrPeerAuthIdentityInvalid
	}
	for i := 0; i < len(identity); i++ {
		if identity[i] < 0x21 || identity[i] > 0x7e {
			return ErrPeerAuthIdentityInvalid
		}
	}
	return nil
}
