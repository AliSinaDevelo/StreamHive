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
	ReconcilePeer  bool
	// BeforeRepair runs after startup watermark reconciliation and before the first repair frame.
	BeforeRepair func(context.Context) error
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
	reconcilePeer  bool
	beforeRepair   func(context.Context) error
	sendMu         sync.Mutex
	ackNotify      chan struct{}
	helloMu        sync.Mutex
	helloReceived  bool
	helloNotify    chan struct{}
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
		reconcilePeer:  options.ReconcilePeer,
		beforeRepair:   options.BeforeRepair,
		ackNotify:      make(chan struct{}, 1),
		helloNotify:    make(chan struct{}, 1),
	}, nil
}

// Run sends one bounded repair sequence and waits for durable acknowledgements
// before planning the next frame. It stops after the current journal tail;
// callers can start it again after a reconnect or when new records exist.
func (s *RepairSession) Run(ctx context.Context) error {
	if s.reconcilePeer {
		if err := s.sendStartupWatermark(ctx); err != nil {
			return err
		}
		if !s.coordinator.journal.LastVersion().IsZero() {
			if err := s.waitForStartupWatermark(ctx); err != nil {
				return err
			}
		}
	}
	if s.beforeRepair != nil {
		if err := s.beforeRepair(ctx); err != nil {
			return err
		}
	}
	for {
		plan, err := s.SendNext(ctx)
		if err != nil {
			return err
		}
		if plan.To.Compare(plan.From) > 0 {
			if err := s.waitForAcknowledgement(ctx, plan.To); err != nil {
				return err
			}
		}
		if !plan.More {
			return nil
		}
	}
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
		if s.reconcilePeer && s.acceptStartupWatermark(frame.Ack.Watermark) {
			if !s.coordinator.journal.LastVersion().IsZero() {
				if err := s.coordinator.Reconcile(ctx, s.peerID, frame.Ack.Watermark); err != nil {
					return err
				}
			}
			return nil
		}
		if err := s.coordinator.Acknowledge(ctx, s.peerID, frame.Ack.Watermark); err != nil {
			return err
		}
		select {
		case s.ackNotify <- struct{}{}:
		default:
		}
		return nil
	case frame.Batch != nil:
		return s.handleBatch(ctx, *frame.Batch)
	case frame.Snapshot != nil:
		return s.handleSnapshot(ctx, *frame.Snapshot)
	default:
		return ErrRepairMalformed
	}
}

func (s *RepairSession) sendStartupWatermark(ctx context.Context) error {
	return s.writeAck(ctx, s.coordinator.watermarks.Watermark(s.peerID))
}

func (s *RepairSession) waitForStartupWatermark(ctx context.Context) error {
	s.helloMu.Lock()
	received := s.helloReceived
	s.helloMu.Unlock()
	if received {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.helloNotify:
		return nil
	}
}

func (s *RepairSession) acceptStartupWatermark(watermark Version) bool {
	s.helloMu.Lock()
	defer s.helloMu.Unlock()
	if s.helloReceived {
		return false
	}
	s.helloReceived = true
	select {
	case s.helloNotify <- struct{}{}:
	default:
	}
	return true
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

func (s *RepairSession) waitForAcknowledgement(ctx context.Context, target Version) error {
	for {
		if s.coordinator.watermarks.Watermark(s.peerID).Compare(target) >= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ackNotify:
		}
	}
}

func (s *RepairSession) requireCapability() error {
	if !HasLifecycleCapability(s.peer.AuthCapabilities()) {
		return ErrLifecycleCapabilityRequired
	}
	return nil
}
