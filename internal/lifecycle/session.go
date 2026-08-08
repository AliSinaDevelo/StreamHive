package lifecycle

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrNilRepairSessionPeer is returned when a session has no frame peer.
	ErrNilRepairSessionPeer = errors.New("lifecycle: nil repair session peer")
	// ErrNilRepairSessionCoordinator is returned when a session has no planner.
	ErrNilRepairSessionCoordinator = errors.New("lifecycle: nil repair session coordinator")
	// ErrNilRepairSessionApplier is returned when a session receives state without an applier.
	ErrNilRepairSessionApplier = errors.New("lifecycle: nil repair session applier")
)

// RepairFramePeer is the minimal authenticated peer surface required by a
// lifecycle repair session. p2p.TCPPeer implements it without a package cycle.
type RepairFramePeer interface {
	AuthCapabilities() []string
	WriteFrame(payload []byte, maxPayload int) error
}

// RepairSessionOptions configures one sender/receiver lifecycle repair session.
type RepairSessionOptions struct {
	Peer           RepairFramePeer
	Coordinator    *RepairCoordinator
	Applier        *Applier
	PeerID         string
	Snapshot       *Checkpoint
	CheckpointPath string
	MaxFrameBytes  int
}

// RepairSession plans, sends, receives, applies, and acknowledges bounded
// lifecycle repair frames for one authenticated peer.
type RepairSession struct {
	peer           RepairFramePeer
	coordinator    *RepairCoordinator
	applier        *Applier
	peerID         string
	snapshot       *Checkpoint
	checkpointPath string
	maxFrameBytes  int
	sendMu         sync.Mutex
}

// NewRepairSession constructs a cancellation-safe session. Applier is optional
// for a sender-only session and required when a batch or snapshot is received.
func NewRepairSession(options RepairSessionOptions) (*RepairSession, error) {
	if options.Peer == nil {
		return nil, ErrNilRepairSessionPeer
	}
	if options.Coordinator == nil {
		return nil, ErrNilRepairSessionCoordinator
	}
	if err := options.Coordinator.watermarks.validatePeer(options.PeerID); err != nil {
		return nil, err
	}
	maxFrameBytes := options.MaxFrameBytes
	if maxFrameBytes <= 0 {
		maxFrameBytes = options.Coordinator.limits.MaxFrameBytes
	}
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxRepairFrameBytes
	}
	return &RepairSession{
		peer:           options.Peer,
		coordinator:    options.Coordinator,
		applier:        options.Applier,
		peerID:         options.PeerID,
		snapshot:       options.Snapshot,
		checkpointPath: options.CheckpointPath,
		maxFrameBytes:  maxFrameBytes,
	}, nil
}

// SendNext plans and writes one bounded repair frame. The watermark advances
// only after the receiver sends a durable acknowledgement back through Handle.
func (s *RepairSession) SendNext(ctx context.Context) (RepairPlan, error) {
	if err := ctx.Err(); err != nil {
		return RepairPlan{}, err
	}
	if err := s.requireCapability(); err != nil {
		return RepairPlan{}, err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	plan, err := s.coordinator.Plan(ctx, s.peerID, s.snapshot)
	if err != nil {
		return RepairPlan{}, err
	}
	if len(plan.Payload) > s.maxFrameBytes {
		return RepairPlan{}, ErrRepairFrameTooLarge
	}
	if err := s.peer.WriteFrame(plan.Payload, s.maxFrameBytes); err != nil {
		return RepairPlan{}, err
	}
	return plan, nil
}

// Handle decodes one incoming repair frame. Received records are applied before
// a watermark acknowledgement is persisted and written to the peer.
func (s *RepairSession) Handle(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireCapability(); err != nil {
		return err
	}
	frame, err := DecodeRepairFrameForPeer(payload, s.peer.AuthCapabilities(), s.coordinator.limits)
	if err != nil {
		return err
	}
	switch {
	case frame.Ack != nil:
		return s.coordinator.Acknowledge(ctx, s.peerID, frame.Ack.Watermark)
	case frame.Batch != nil:
		return s.handleBatch(ctx, *frame.Batch)
	case frame.Snapshot != nil:
		return s.handleSnapshot(ctx, *frame.Snapshot)
	default:
		return ErrRepairMalformed
	}
}

func (s *RepairSession) handleBatch(ctx context.Context, batch RepairBatch) error {
	if s.applier == nil {
		return ErrNilRepairSessionApplier
	}
	expected := s.coordinator.watermarks.Watermark(s.peerID)
	delivery, err := batch.Delivery(expected, s.coordinator.limits)
	if err != nil {
		return err
	}
	if delivery == RepairDeliveryDuplicate {
		return s.writeAck(ctx, expected)
	}
	capabilities := s.peer.AuthCapabilities()
	for _, record := range batch.Records {
		if _, err := s.applier.Apply(ctx, capabilities, record, nil); err != nil {
			return err
		}
	}
	if err := s.coordinator.Acknowledge(ctx, s.peerID, batch.To); err != nil {
		return err
	}
	return s.writeAck(ctx, batch.To)
}

func (s *RepairSession) handleSnapshot(ctx context.Context, snapshot RepairSnapshot) error {
	if s.applier == nil {
		return ErrNilRepairSessionApplier
	}
	expected := s.coordinator.watermarks.Watermark(s.peerID)
	if snapshot.Watermark.Compare(expected) <= 0 {
		return s.writeAck(ctx, expected)
	}
	if err := s.applier.ApplySnapshot(ctx, s.peer.AuthCapabilities(), Checkpoint{
		Watermark: snapshot.Watermark,
		Records:   snapshot.Records,
	}, s.checkpointPath); err != nil {
		return err
	}
	if err := s.coordinator.Acknowledge(ctx, s.peerID, snapshot.Watermark); err != nil {
		return err
	}
	return s.writeAck(ctx, snapshot.Watermark)
}

func (s *RepairSession) writeAck(ctx context.Context, watermark Version) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := EncodeRepairAck(RepairAck{
		Type:      RepairAckMessageType,
		Watermark: watermark,
	}, s.coordinator.limits)
	if err != nil {
		return err
	}
	return s.peer.WriteFrame(payload, s.maxFrameBytes)
}

func (s *RepairSession) requireCapability() error {
	if !HasLifecycleCapability(s.peer.AuthCapabilities()) {
		return ErrLifecycleCapabilityRequired
	}
	return nil
}
