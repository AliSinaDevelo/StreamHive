package p2p

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePeerAuthCapabilities(t *testing.T) {
	tests := []struct {
		name          string
		capabilities  []string
		rejectUnknown bool
		want          []string
		wantErr       error
	}{
		{name: "absent", want: []string{}},
		{
			name:         "unknown incoming capability is ignored",
			capabilities: []string{"future.v9"},
			want:         []string{},
		},
		{
			name:          "unknown local capability is rejected",
			capabilities:  []string{"future.v9"},
			rejectUnknown: true,
			wantErr:       ErrPeerAuthCapabilityUnknown,
		},
		{
			name:         "known lifecycle repair reconciliation capability",
			capabilities: []string{CapabilityLifecycleRepairReconcileV1, CapabilityLifecycleV1},
			want:         []string{CapabilityLifecycleRepairReconcileV1, CapabilityLifecycleV1},
		},
		{
			name:         "duplicate capability",
			capabilities: []string{CapabilityLifecycleV1, CapabilityLifecycleV1},
			wantErr:      ErrPeerAuthCapabilityDuplicate,
		},
		{
			name:         "too many capabilities",
			capabilities: []string{},
			wantErr:      ErrPeerAuthCapabilitiesTooMany,
		},
		{
			name:         "capability too large",
			capabilities: []string{string(bytes.Repeat([]byte{'x'}, MaxPeerAuthCapabilityBytes+1))},
			wantErr:      ErrPeerAuthCapabilityTooLarge,
		},
		{
			name:         "capability invalid",
			capabilities: []string{"future\n"},
			wantErr:      ErrPeerAuthCapabilityInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capabilities := tt.capabilities
			if tt.name == "too many capabilities" {
				capabilities = make([]string, MaxPeerAuthCapabilities+1)
				for i := range capabilities {
					capabilities[i] = "future." + string(rune('a'+i))
				}
			}
			got, err := normalizePeerAuthCapabilities(capabilities, tt.rejectUnknown)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizePeerAuthCapabilitiesRejectsAggregateLimit(t *testing.T) {
	capabilities := make([]string, 9)
	for i := range capabilities {
		capabilities[i] = string(append(bytes.Repeat([]byte{'x'}, MaxPeerAuthCapabilityBytes-1), byte('a'+i)))
	}
	_, err := normalizePeerAuthCapabilities(capabilities, false)
	assert.ErrorIs(t, err, ErrPeerAuthCapabilitiesTooLarge)
}

func TestValidatePeerAuthCapabilitiesCompatibility(t *testing.T) {
	payload, err := json.Marshal(peerAuthMessage{
		Type:         handshakeTypePeerAuth,
		Version:      HandshakeVersionV1,
		Token:        "shared-secret",
		Capabilities: []string{"future.v9"},
	})
	require.NoError(t, err)

	identity, capabilities, err := validatePeerAuthPayload(payload, "shared-secret", nil)
	require.NoError(t, err)
	assert.Empty(t, identity)
	assert.Empty(t, capabilities)

	duplicate, err := json.Marshal(peerAuthMessage{
		Type:         handshakeTypePeerAuth,
		Version:      HandshakeVersionV1,
		Token:        "shared-secret",
		Capabilities: []string{CapabilityLifecycleV1, CapabilityLifecycleV1},
	})
	require.NoError(t, err)
	_, _, err = validatePeerAuthPayload(duplicate, "shared-secret", nil)
	assert.ErrorIs(t, err, ErrPeerAuthCapabilityDuplicate)
}

func TestValidatePeerAuthPayloadBoundsBeforeDecode(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, MaxPeerAuthPayloadBytes+1)
	_, _, err := validatePeerAuthPayload(payload, "shared-secret", nil)
	assert.ErrorIs(t, err, ErrPeerAuthPayloadTooLarge)
}

func TestLifecycleCapabilityStatus(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []string
		required     bool
		want         CapabilityStatus
	}{
		{name: "negotiated", capabilities: []string{CapabilityLifecycleV1}, required: true, want: CapabilityStatusReady},
		{name: "optional absent", required: false, want: CapabilityStatusOptionalRawOnly},
		{name: "required absent", required: true, want: CapabilityStatusRequiredUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LifecycleCapabilityStatus(tt.capabilities, tt.required))
		})
	}
}

func TestEncodePeerAuthRejectsUnknownLocalCapability(t *testing.T) {
	_, err := encodePeerAuth("shared-secret", "node-a", []string{"future.v9"})
	assert.ErrorIs(t, err, ErrPeerAuthCapabilityUnknown)
}

func TestValidatePeerAuthAckRejectsOversizedPayload(t *testing.T) {
	_, _, err := validatePeerAuthAck(bytes.Repeat([]byte{'x'}, MaxPeerAuthPayloadBytes+1))
	assert.True(t, errors.Is(err, ErrPeerAuthPayloadTooLarge))
}
