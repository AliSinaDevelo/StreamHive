package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const (
	// RepairBatchMessageType identifies an ordered journal tail batch.
	RepairBatchMessageType = "lifecycle.repair.batch"
	// RepairSnapshotMessageType identifies a complete checkpoint bootstrap.
	RepairSnapshotMessageType = "lifecycle.repair.snapshot"
	// RepairAckMessageType identifies an acknowledgement of an applied watermark.
	RepairAckMessageType = "lifecycle.repair.ack"
	// DefaultMaxRepairRecords bounds one repair batch or snapshot.
	DefaultMaxRepairRecords = 128
	// DefaultMaxRepairLogicalKeyBytes bounds logical namespace/key bytes in one repair payload.
	DefaultMaxRepairLogicalKeyBytes = 64 << 10
	// DefaultMaxRepairMetadataBytes bounds encoded lifecycle metadata in one repair payload.
	DefaultMaxRepairMetadataBytes = 64 << 10
	// DefaultMaxRepairFrameBytes bounds one repair JSON payload independently from raw blobs.
	DefaultMaxRepairFrameBytes = 128 << 10
)

var (
	// ErrNilRepairJournal is returned when a repair planner has no journal.
	ErrNilRepairJournal = errors.New("lifecycle: nil repair journal")
	// ErrRepairLimit is returned when a repair batch exceeds its entry or metadata bounds.
	ErrRepairLimit = errors.New("lifecycle: repair limit exceeded")
	// ErrRepairFrameTooLarge is returned when a repair payload exceeds its frame bound.
	ErrRepairFrameTooLarge = errors.New("lifecycle: repair frame too large")
	// ErrRepairMalformed is returned for invalid repair JSON or envelope structure.
	ErrRepairMalformed = errors.New("lifecycle: malformed repair envelope")
	// ErrRepairMessageType is returned for a repair envelope of another kind.
	ErrRepairMessageType = errors.New("lifecycle: unexpected repair message type")
	// ErrRepairOrder is returned when a batch is not strictly ordered after its watermark.
	ErrRepairOrder = errors.New("lifecycle: repair records are not ordered")
	// ErrRepairWatermarkMismatch is returned for a gap or inconsistent batch endpoint.
	ErrRepairWatermarkMismatch = errors.New("lifecycle: repair watermark mismatch")
	// ErrRepairSnapshotRequired is returned when the journal floor is newer than a peer.
	ErrRepairSnapshotRequired = errors.New("lifecycle: repair snapshot required")
	// ErrRepairSnapshotWatermark is returned when a fallback checkpoint does not match the floor.
	ErrRepairSnapshotWatermark = errors.New("lifecycle: repair snapshot watermark mismatch")
)

// RepairLimits bounds one ordered batch or snapshot.
type RepairLimits struct {
	MaxRecords         int
	MaxLogicalKeyBytes int
	MaxMetadataBytes   int
	MaxFrameBytes      int
	RecordLimits       Limits
}

func (l RepairLimits) normalized() RepairLimits {
	if l.MaxRecords <= 0 {
		l.MaxRecords = DefaultMaxRepairRecords
	}
	if l.MaxLogicalKeyBytes <= 0 {
		l.MaxLogicalKeyBytes = DefaultMaxRepairLogicalKeyBytes
	}
	if l.MaxMetadataBytes <= 0 {
		l.MaxMetadataBytes = DefaultMaxRepairMetadataBytes
	}
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = DefaultMaxRepairFrameBytes
	}
	l.RecordLimits = l.RecordLimits.normalized()
	return l
}

// RepairBatch is an ordered journal slice after From and through To.
type RepairBatch struct {
	Type    string   `json:"type"`
	From    Version  `json:"from"`
	To      Version  `json:"to"`
	More    bool     `json:"more"`
	Records []Record `json:"records"`
}

// RepairSnapshot is a complete logical checkpoint at Watermark.
type RepairSnapshot struct {
	Type      string   `json:"type"`
	Watermark Version  `json:"watermark"`
	Records   []Record `json:"records"`
}

// RepairAck confirms that a receiver durably applied through Watermark.
type RepairAck struct {
	Type      string  `json:"type"`
	Watermark Version `json:"watermark"`
}

// RepairFrame is the bounded lifecycle repair message union used at the wire boundary.
type RepairFrame struct {
	Type     string
	Batch    *RepairBatch
	Snapshot *RepairSnapshot
	Ack      *RepairAck
}

// RepairPlanMode identifies whether a peer receives journal tail or snapshot state.
type RepairPlanMode string

const (
	RepairPlanJournal  RepairPlanMode = "journal"
	RepairPlanSnapshot RepairPlanMode = "snapshot"
)

// RepairPlan is a bounded payload and its resulting watermark.
type RepairPlan struct {
	Mode    RepairPlanMode
	From    Version
	To      Version
	More    bool
	Payload []byte
}

// RepairDelivery classifies a decoded batch relative to the receiver watermark.
type RepairDelivery string

const (
	RepairDeliveryReady     RepairDelivery = "ready"
	RepairDeliveryDuplicate RepairDelivery = "duplicate"
)

// EncodeRepairBatch validates and encodes an ordered journal batch.
func EncodeRepairBatch(batch RepairBatch, limits RepairLimits) ([]byte, error) {
	limits = limits.normalized()
	if batch.Type == "" {
		batch.Type = RepairBatchMessageType
	}
	if batch.Type != RepairBatchMessageType {
		return nil, ErrRepairMessageType
	}
	if err := validateRepairBatch(batch, limits); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return nil, errors.Join(ErrRepairMalformed, err)
	}
	if len(payload) > limits.MaxFrameBytes {
		return nil, ErrRepairFrameTooLarge
	}
	return payload, nil
}

// DecodeRepairBatch validates one ordered journal batch without applying records.
func DecodeRepairBatch(payload []byte, limits RepairLimits) (RepairBatch, error) {
	limits = limits.normalized()
	if len(payload) > limits.MaxFrameBytes {
		return RepairBatch{}, ErrRepairFrameTooLarge
	}
	var batch RepairBatch
	if err := decodeRepairEnvelope(payload, &batch); err != nil {
		return RepairBatch{}, err
	}
	if batch.Type != RepairBatchMessageType {
		return RepairBatch{}, ErrRepairMessageType
	}
	if err := validateRepairBatch(batch, limits); err != nil {
		return RepairBatch{}, err
	}
	return batch, nil
}

// EncodeRepairSnapshot validates and encodes a complete checkpoint bootstrap.
func EncodeRepairSnapshot(snapshot RepairSnapshot, limits RepairLimits) ([]byte, error) {
	limits = limits.normalized()
	if snapshot.Type == "" {
		snapshot.Type = RepairSnapshotMessageType
	}
	if snapshot.Type != RepairSnapshotMessageType {
		return nil, ErrRepairMessageType
	}
	normalized, err := normalizeRepairSnapshot(snapshot, limits)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return nil, errors.Join(ErrRepairMalformed, err)
	}
	if len(payload) > limits.MaxFrameBytes {
		return nil, ErrRepairFrameTooLarge
	}
	return payload, nil
}

// DecodeRepairSnapshot validates one complete checkpoint bootstrap.
func DecodeRepairSnapshot(payload []byte, limits RepairLimits) (RepairSnapshot, error) {
	limits = limits.normalized()
	if len(payload) > limits.MaxFrameBytes {
		return RepairSnapshot{}, ErrRepairFrameTooLarge
	}
	var snapshot RepairSnapshot
	if err := decodeRepairEnvelope(payload, &snapshot); err != nil {
		return RepairSnapshot{}, err
	}
	if snapshot.Type != RepairSnapshotMessageType {
		return RepairSnapshot{}, ErrRepairMessageType
	}
	return normalizeRepairSnapshot(snapshot, limits)
}

// EncodeRepairAck validates and encodes one durable watermark acknowledgement.
func EncodeRepairAck(ack RepairAck, limits RepairLimits) ([]byte, error) {
	limits = limits.normalized()
	if ack.Type == "" {
		ack.Type = RepairAckMessageType
	}
	if ack.Type != RepairAckMessageType {
		return nil, ErrRepairMessageType
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		return nil, errors.Join(ErrRepairMalformed, err)
	}
	if len(payload) > limits.MaxFrameBytes {
		return nil, ErrRepairFrameTooLarge
	}
	return payload, nil
}

// DecodeRepairAck validates one durable watermark acknowledgement.
func DecodeRepairAck(payload []byte, limits RepairLimits) (RepairAck, error) {
	limits = limits.normalized()
	if len(payload) > limits.MaxFrameBytes {
		return RepairAck{}, ErrRepairFrameTooLarge
	}
	var ack RepairAck
	if err := decodeRepairEnvelope(payload, &ack); err != nil {
		return RepairAck{}, err
	}
	if ack.Type != RepairAckMessageType {
		return RepairAck{}, ErrRepairMessageType
	}
	return ack, nil
}

// EncodeRepairFrame encodes exactly one lifecycle repair message.
func EncodeRepairFrame(frame RepairFrame, limits RepairLimits) ([]byte, error) {
	var count int
	if frame.Batch != nil {
		count++
	}
	if frame.Snapshot != nil {
		count++
	}
	if frame.Ack != nil {
		count++
	}
	if count != 1 {
		return nil, ErrRepairMalformed
	}
	switch {
	case frame.Batch != nil:
		if frame.Type != "" && frame.Type != RepairBatchMessageType {
			return nil, ErrRepairMessageType
		}
		return EncodeRepairBatch(*frame.Batch, limits)
	case frame.Snapshot != nil:
		if frame.Type != "" && frame.Type != RepairSnapshotMessageType {
			return nil, ErrRepairMessageType
		}
		return EncodeRepairSnapshot(*frame.Snapshot, limits)
	case frame.Ack != nil:
		if frame.Type != "" && frame.Type != RepairAckMessageType {
			return nil, ErrRepairMessageType
		}
		return EncodeRepairAck(*frame.Ack, limits)
	default:
		return nil, ErrRepairMalformed
	}
}

// DecodeRepairFrame validates one lifecycle repair message without applying it.
func DecodeRepairFrame(payload []byte, limits RepairLimits) (RepairFrame, error) {
	limits = limits.normalized()
	if len(payload) > limits.MaxFrameBytes {
		return RepairFrame{}, ErrRepairFrameTooLarge
	}
	typeName, err := decodeRepairType(payload)
	if err != nil {
		return RepairFrame{}, err
	}
	switch typeName {
	case RepairBatchMessageType:
		batch, err := DecodeRepairBatch(payload, limits)
		if err != nil {
			return RepairFrame{}, err
		}
		return RepairFrame{Type: typeName, Batch: &batch}, nil
	case RepairSnapshotMessageType:
		snapshot, err := DecodeRepairSnapshot(payload, limits)
		if err != nil {
			return RepairFrame{}, err
		}
		return RepairFrame{Type: typeName, Snapshot: &snapshot}, nil
	case RepairAckMessageType:
		ack, err := DecodeRepairAck(payload, limits)
		if err != nil {
			return RepairFrame{}, err
		}
		return RepairFrame{Type: typeName, Ack: &ack}, nil
	default:
		return RepairFrame{}, ErrRepairMessageType
	}
}

// DecodeRepairFrameForPeer refuses lifecycle repair data before decoding when
// the remote peer lacks lifecycle.v1.
func DecodeRepairFrameForPeer(payload []byte, capabilities []string, limits RepairLimits) (RepairFrame, error) {
	if !HasLifecycleCapability(capabilities) {
		return RepairFrame{}, ErrLifecycleCapabilityRequired
	}
	return DecodeRepairFrame(payload, limits)
}

// Delivery classifies a batch as ready, a harmless duplicate, or a gap.
func (b RepairBatch) Delivery(expected Version, limits RepairLimits) (RepairDelivery, error) {
	limits = limits.normalized()
	if err := validateRepairBatch(b, limits); err != nil {
		return "", err
	}
	if b.To.Compare(expected) <= 0 {
		return RepairDeliveryDuplicate, nil
	}
	if b.From != expected {
		return "", ErrRepairWatermarkMismatch
	}
	return RepairDeliveryReady, nil
}

// RepairBatch returns the next bounded journal tail after a peer watermark.
func (j *Journal) RepairBatch(ctx context.Context, from Version, limits RepairLimits) (RepairBatch, error) {
	if err := ctx.Err(); err != nil {
		return RepairBatch{}, err
	}
	if j == nil {
		return RepairBatch{}, ErrNilRepairJournal
	}
	limits = limits.normalized()
	j.mu.Lock()
	if err := j.ensureOpenLocked(); err != nil {
		j.mu.Unlock()
		return RepairBatch{}, err
	}
	if from.Compare(j.floor) < 0 {
		j.mu.Unlock()
		return RepairBatch{}, ErrPeerBehindWatermark
	}
	records := cloneRecords(j.records)
	j.mu.Unlock()

	remaining := make([]Record, 0, len(records))
	for _, record := range records {
		if record.Version.Compare(from) > 0 {
			remaining = append(remaining, record)
		}
	}
	selected := make([]Record, 0, len(remaining))
	for _, record := range remaining {
		candidate := append(cloneRecords(selected), record)
		batch := RepairBatch{
			Type:    RepairBatchMessageType,
			From:    from,
			To:      record.Version,
			More:    len(candidate) < len(remaining),
			Records: candidate,
		}
		if err := validateRepairBatch(batch, limits); err != nil {
			if len(selected) == 0 {
				return RepairBatch{}, err
			}
			break
		}
		if _, err := EncodeRepairBatch(batch, limits); err != nil {
			if len(selected) == 0 {
				return RepairBatch{}, err
			}
			break
		}
		selected = candidate
	}
	batch := RepairBatch{Type: RepairBatchMessageType, From: from, To: from, Records: selected}
	if len(selected) > 0 {
		batch.To = selected[len(selected)-1].Version
	}
	batch.More = len(selected) < len(remaining)
	if _, err := EncodeRepairBatch(batch, limits); err != nil {
		return RepairBatch{}, err
	}
	return batch, nil
}

// PlanRepair chooses a bounded journal batch or a checkpoint fallback.
func PlanRepair(ctx context.Context, journal *Journal, from Version, snapshot *Checkpoint, limits RepairLimits) (RepairPlan, error) {
	if journal == nil {
		return RepairPlan{}, ErrNilRepairJournal
	}
	if err := ctx.Err(); err != nil {
		return RepairPlan{}, err
	}
	limits = limits.normalized()
	floor := journal.Floor()
	if from.Compare(floor) < 0 {
		if snapshot == nil {
			return RepairPlan{}, ErrRepairSnapshotRequired
		}
		if snapshot.Watermark != floor {
			return RepairPlan{}, ErrRepairSnapshotWatermark
		}
		payload, err := EncodeRepairSnapshot(RepairSnapshot{
			Type:      RepairSnapshotMessageType,
			Watermark: snapshot.Watermark,
			Records:   snapshot.Records,
		}, limits)
		if err != nil {
			return RepairPlan{}, err
		}
		return RepairPlan{
			Mode:    RepairPlanSnapshot,
			From:    from,
			To:      floor,
			Payload: payload,
		}, nil
	}
	batch, err := journal.RepairBatch(ctx, from, limits)
	if err != nil {
		return RepairPlan{}, err
	}
	payload, err := EncodeRepairBatch(batch, limits)
	if err != nil {
		return RepairPlan{}, err
	}
	return RepairPlan{
		Mode:    RepairPlanJournal,
		From:    batch.From,
		To:      batch.To,
		More:    batch.More,
		Payload: payload,
	}, nil
}

func validateRepairBatch(batch RepairBatch, limits RepairLimits) error {
	if len(batch.Records) > limits.MaxRecords {
		return ErrRepairLimit
	}
	previous := batch.From
	var logicalBytes, metadataBytes int
	for _, record := range batch.Records {
		if record.Version.Compare(previous) <= 0 {
			return ErrRepairOrder
		}
		if err := record.Validate(limits.RecordLimits); err != nil {
			return err
		}
		logical, metadata, err := repairRecordBytes(record)
		if err != nil {
			return errors.Join(ErrRepairMalformed, err)
		}
		logicalBytes += logical
		metadataBytes += metadata
		if logicalBytes > limits.MaxLogicalKeyBytes || metadataBytes > limits.MaxMetadataBytes {
			return ErrRepairLimit
		}
		previous = record.Version
	}
	if batch.To != previous {
		return ErrRepairWatermarkMismatch
	}
	return nil
}

func normalizeRepairSnapshot(snapshot RepairSnapshot, limits RepairLimits) (RepairSnapshot, error) {
	checkpoint, _, err := normalizeCheckpoint(Checkpoint{
		Watermark: snapshot.Watermark,
		Records:   snapshot.Records,
	}, limits.RecordLimits)
	if err != nil {
		return RepairSnapshot{}, err
	}
	if len(checkpoint.Records) > limits.MaxRecords {
		return RepairSnapshot{}, ErrRepairLimit
	}
	var logicalBytes, metadataBytes int
	for _, record := range checkpoint.Records {
		logical, metadata, err := repairRecordBytes(record)
		if err != nil {
			return RepairSnapshot{}, errors.Join(ErrRepairMalformed, err)
		}
		logicalBytes += logical
		metadataBytes += metadata
		if logicalBytes > limits.MaxLogicalKeyBytes || metadataBytes > limits.MaxMetadataBytes {
			return RepairSnapshot{}, ErrRepairLimit
		}
	}
	snapshot.Type = RepairSnapshotMessageType
	snapshot.Records = checkpoint.Records
	return snapshot, nil
}

func repairRecordBytes(record Record) (int, int, error) {
	metadata, err := json.Marshal(record)
	if err != nil {
		return 0, 0, err
	}
	return len(record.Namespace) + len(record.LogicalKey), len(metadata), nil
}

func decodeRepairEnvelope(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.Join(ErrRepairMalformed, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.Join(ErrRepairMalformed, ErrRepairWatermarkMismatch)
		}
		return errors.Join(ErrRepairMalformed, err)
	}
	return nil
}

func decodeRepairType(payload []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var envelope struct {
		Type string `json:"type"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		return "", errors.Join(ErrRepairMalformed, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", ErrRepairMalformed
		}
		return "", errors.Join(ErrRepairMalformed, err)
	}
	if envelope.Type == "" {
		return "", ErrRepairMalformed
	}
	return envelope.Type, nil
}
