package p2p

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sort"
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

// DefaultTLSHandshakeTimeout bounds TLS handshakes before peer registration.
const DefaultTLSHandshakeTimeout = 5 * time.Second

// MaxPeerAuthIdentityBytes bounds the application identity carried in auth frames.
const MaxPeerAuthIdentityBytes = 128

const (
	// CapabilityLifecycleV1 identifies the first logical lifecycle record protocol.
	CapabilityLifecycleV1 = "lifecycle.v1"
	// MaxPeerAuthCapabilities bounds the number of declarations in one auth frame.
	MaxPeerAuthCapabilities = 16
	// MaxPeerAuthCapabilityBytes bounds one capability declaration.
	MaxPeerAuthCapabilityBytes = 64
	// MaxPeerAuthCapabilitiesBytes bounds the aggregate declaration bytes.
	MaxPeerAuthCapabilitiesBytes = 512
	// MaxPeerAuthPayloadBytes bounds auth frames independently from replication frames.
	MaxPeerAuthPayloadBytes = 4 << 10
)

var (
	// ErrPeerAuthFailed is returned when the peer auth handshake is malformed or incomplete.
	ErrPeerAuthFailed = errors.New("p2p: peer auth failed")
	// ErrPeerAuthRejected is returned when a peer presents an invalid auth token.
	ErrPeerAuthRejected = errors.New("p2p: peer auth rejected")
	// ErrPeerAuthIdentityInvalid is returned when an application identity is malformed.
	ErrPeerAuthIdentityInvalid = errors.New("p2p: peer auth identity invalid")
	// ErrPeerAuthIdentityRequiresToken is returned when identity exchange is configured without shared-token auth.
	ErrPeerAuthIdentityRequiresToken = errors.New("p2p: peer auth identity requires peer auth token")
	// ErrPeerAuthIdentityNotAllowed is returned when an identity is absent from the inbound allowlist.
	ErrPeerAuthIdentityNotAllowed = errors.New("p2p: peer auth identity not allowed")
	// ErrPeerAuthPayloadTooLarge is returned when an auth frame exceeds its independent bound.
	ErrPeerAuthPayloadTooLarge = errors.New("p2p: peer auth payload too large")
	// ErrPeerAuthCapabilitiesRequiresToken is returned when capabilities are configured without auth.
	ErrPeerAuthCapabilitiesRequiresToken = errors.New("p2p: peer auth capabilities require peer auth token")
	// ErrPeerAuthCapabilitiesTooMany is returned when an auth frame declares too many capabilities.
	ErrPeerAuthCapabilitiesTooMany = errors.New("p2p: too many peer auth capabilities")
	// ErrPeerAuthCapabilityTooLarge is returned when one capability exceeds its bound.
	ErrPeerAuthCapabilityTooLarge = errors.New("p2p: peer auth capability too large")
	// ErrPeerAuthCapabilitiesTooLarge is returned when capability declarations exceed their aggregate bound.
	ErrPeerAuthCapabilitiesTooLarge = errors.New("p2p: peer auth capabilities too large")
	// ErrPeerAuthCapabilityInvalid is returned when a capability contains unsupported bytes.
	ErrPeerAuthCapabilityInvalid = errors.New("p2p: peer auth capability invalid")
	// ErrPeerAuthCapabilityDuplicate is returned when a capability is declared more than once.
	ErrPeerAuthCapabilityDuplicate = errors.New("p2p: duplicate peer auth capability")
	// ErrPeerAuthCapabilityUnknown is returned for an unsupported locally configured capability.
	ErrPeerAuthCapabilityUnknown = errors.New("p2p: unknown peer auth capability")
)

type peerAuthMessage struct {
	Type         string   `json:"type"`
	Version      string   `json:"version"`
	Token        string   `json:"token,omitempty"`
	Identity     string   `json:"identity,omitempty"`
	Error        string   `json:"error,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func encodePeerAuth(token, identity string, capabilities []string) ([]byte, error) {
	normalized, err := normalizePeerAuthCapabilities(capabilities, true)
	if err != nil {
		return nil, err
	}
	return marshalPeerAuthMessage(peerAuthMessage{
		Type:         handshakeTypePeerAuth,
		Version:      HandshakeVersionV1,
		Token:        token,
		Identity:     identity,
		Capabilities: normalized,
	})
}

func encodePeerAuthOK(identity string, capabilities []string) ([]byte, error) {
	normalized, err := normalizePeerAuthCapabilities(capabilities, true)
	if err != nil {
		return nil, err
	}
	return marshalPeerAuthMessage(peerAuthMessage{
		Type:         handshakeTypePeerAuthOK,
		Version:      HandshakeVersionV1,
		Identity:     identity,
		Capabilities: normalized,
	})
}

func encodePeerAuthReject() ([]byte, error) {
	return marshalPeerAuthMessage(peerAuthMessage{
		Type:    handshakeTypePeerAuthReject,
		Version: HandshakeVersionV1,
		Error:   "unauthorized",
	})
}

func marshalPeerAuthMessage(message peerAuthMessage) ([]byte, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxPeerAuthPayloadBytes {
		return nil, ErrPeerAuthPayloadTooLarge
	}
	return payload, nil
}

func validatePeerAuthPayload(payload []byte, token string, allowedIdentities []string) (string, []string, error) {
	if len(payload) > MaxPeerAuthPayloadBytes {
		return "", nil, ErrPeerAuthPayloadTooLarge
	}
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", nil, errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Type != handshakeTypePeerAuth || msg.Version != HandshakeVersionV1 {
		return "", nil, ErrPeerAuthFailed
	}
	if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(token)) != 1 {
		return "", nil, ErrPeerAuthRejected
	}
	if err := validatePeerIdentity(msg.Identity); err != nil {
		return "", nil, err
	}
	if len(allowedIdentities) > 0 && !peerAuthIdentityAllowed(msg.Identity, allowedIdentities) {
		return "", nil, ErrPeerAuthIdentityNotAllowed
	}
	capabilities, err := normalizePeerAuthCapabilities(msg.Capabilities, false)
	if err != nil {
		return "", nil, err
	}
	return msg.Identity, capabilities, nil
}

func validatePeerAuthAck(payload []byte) (string, []string, error) {
	if len(payload) > MaxPeerAuthPayloadBytes {
		return "", nil, ErrPeerAuthPayloadTooLarge
	}
	var msg peerAuthMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", nil, errors.Join(ErrPeerAuthFailed, err)
	}
	if msg.Version != HandshakeVersionV1 {
		return "", nil, ErrPeerAuthFailed
	}
	switch msg.Type {
	case handshakeTypePeerAuthOK:
		if err := validatePeerIdentity(msg.Identity); err != nil {
			return "", nil, err
		}
		capabilities, err := normalizePeerAuthCapabilities(msg.Capabilities, false)
		if err != nil {
			return "", nil, err
		}
		return msg.Identity, capabilities, nil
	case handshakeTypePeerAuthReject:
		return "", nil, ErrPeerAuthRejected
	default:
		return "", nil, ErrPeerAuthFailed
	}
}

func normalizePeerAuthCapabilities(capabilities []string, rejectUnknown bool) ([]string, error) {
	if len(capabilities) > MaxPeerAuthCapabilities {
		return nil, ErrPeerAuthCapabilitiesTooMany
	}
	seen := make(map[string]struct{}, len(capabilities))
	var totalBytes int
	normalized := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if len(capability) > MaxPeerAuthCapabilityBytes {
			return nil, ErrPeerAuthCapabilityTooLarge
		}
		totalBytes += len(capability)
		if totalBytes > MaxPeerAuthCapabilitiesBytes {
			return nil, ErrPeerAuthCapabilitiesTooLarge
		}
		if capability == "" {
			return nil, ErrPeerAuthCapabilityInvalid
		}
		for i := 0; i < len(capability); i++ {
			if capability[i] < 0x21 || capability[i] > 0x7e {
				return nil, ErrPeerAuthCapabilityInvalid
			}
		}
		if _, exists := seen[capability]; exists {
			return nil, ErrPeerAuthCapabilityDuplicate
		}
		seen[capability] = struct{}{}
		if capability != CapabilityLifecycleV1 {
			if rejectUnknown {
				return nil, ErrPeerAuthCapabilityUnknown
			}
			continue
		}
		normalized = append(normalized, capability)
	}
	sort.Strings(normalized)
	return normalized, nil
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

func validatePeerAuthAllowlist(identities []string) error {
	for _, identity := range identities {
		if identity == "" {
			return ErrPeerAuthIdentityInvalid
		}
		if err := validatePeerIdentity(identity); err != nil {
			return err
		}
	}
	return nil
}

func peerAuthIdentityAllowed(identity string, allowedIdentities []string) bool {
	for _, allowedIdentity := range allowedIdentities {
		if identity == allowedIdentity {
			return true
		}
	}
	return false
}
