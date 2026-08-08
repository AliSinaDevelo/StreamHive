package p2p

// CapabilityStatus describes whether a capability can be used for a peer.
type CapabilityStatus string

const (
	// CapabilityStatusReady means both peers negotiated the capability.
	CapabilityStatusReady CapabilityStatus = "ready"
	// CapabilityStatusOptionalRawOnly means the capability is absent but raw replication may continue.
	CapabilityStatusOptionalRawOnly CapabilityStatus = "optional-raw-only"
	// CapabilityStatusRequiredUnavailable means a required capability is absent.
	CapabilityStatusRequiredUnavailable CapabilityStatus = "required-unavailable"
)

// HasCapability reports whether a capability is present in a negotiated set.
func HasCapability(capabilities []string, capability string) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

// LifecycleCapabilityStatus classifies lifecycle readiness without changing raw blob behavior.
func LifecycleCapabilityStatus(capabilities []string, required bool) CapabilityStatus {
	if HasCapability(capabilities, CapabilityLifecycleV1) {
		return CapabilityStatusReady
	}
	if required {
		return CapabilityStatusRequiredUnavailable
	}
	return CapabilityStatusOptionalRawOnly
}
