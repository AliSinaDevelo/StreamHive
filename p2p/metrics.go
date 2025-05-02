package p2p

import "sync/atomic"

// TransportMetrics holds atomic counters for observability (logs, /metrics JSON, Prometheus adapters).
type TransportMetrics struct {
	InboundAccepts   atomic.Uint64
	AcceptErrors     atomic.Uint64
	DialAttempts     atomic.Uint64
	DialSuccess      atomic.Uint64
	DialErrors       atomic.Uint64
	PeersRejected    atomic.Uint64
	ActivePeers      atomic.Int64
	FramesHandled    atomic.Uint64
	FrameHandlerErrs atomic.Uint64
}

// NewTransportMetrics returns a zeroed metrics struct.
func NewTransportMetrics() *TransportMetrics {
	return &TransportMetrics{}
}

// Snapshot returns a point-in-time copy suitable for JSON or logs.
func (m *TransportMetrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"inbound_accepts":      int64(m.InboundAccepts.Load()),
		"accept_errors":        int64(m.AcceptErrors.Load()),
		"dial_attempts":        int64(m.DialAttempts.Load()),
		"dial_success":         int64(m.DialSuccess.Load()),
		"dial_errors":          int64(m.DialErrors.Load()),
		"peers_rejected":       int64(m.PeersRejected.Load()),
		"active_peers":         m.ActivePeers.Load(),
		"frames_handled":       int64(m.FramesHandled.Load()),
		"frame_handler_errors": int64(m.FrameHandlerErrs.Load()),
	}
}
