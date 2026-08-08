package lifecycle

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const (
	journalHeaderBytes = 28
	journalFormat      = 1
	journalMagic       = "SHLJ"
)

var (
	ErrJournalClosed          = errors.New("lifecycle: journal closed")
	ErrJournalCorrupt         = errors.New("lifecycle: corrupt journal")
	ErrJournalChecksum        = errors.New("lifecycle: journal checksum mismatch")
	ErrJournalVersionOrder    = errors.New("lifecycle: journal version is not increasing")
	ErrJournalLimit           = errors.New("lifecycle: journal limit exceeded")
	ErrCheckpointCorrupt      = errors.New("lifecycle: corrupt checkpoint")
	ErrCheckpointChecksum     = errors.New("lifecycle: checkpoint checksum mismatch")
	ErrCheckpointTooLarge     = errors.New("lifecycle: checkpoint too large")
	ErrCompactionUnsafe       = errors.New("lifecycle: unsafe compaction")
	ErrCompactionNoProgress   = errors.New("lifecycle: compaction watermark does not advance")
	ErrCompactionWatermark    = errors.New("lifecycle: invalid compaction watermark")
	ErrCompactionBaseMismatch = errors.New("lifecycle: compaction base mismatch")
	ErrPeerBehindWatermark    = errors.New("lifecycle: peer is behind compaction watermark")
	ErrSnapshotStale          = errors.New("lifecycle: snapshot is older than journal")
	ErrNilCheckpointPath      = errors.New("lifecycle: empty checkpoint path")
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// JournalOptions bounds the durable journal.
type JournalOptions struct {
	Limits            Limits
	MaxJournalBytes   int64
	MaxJournalEntries int
}

func (o JournalOptions) normalized() (JournalOptions, error) {
	o.Limits = o.Limits.normalized()
	if o.MaxJournalBytes <= 0 {
		o.MaxJournalBytes = DefaultMaxJournalBytes
	}
	if o.MaxJournalEntries <= 0 {
		o.MaxJournalEntries = DefaultMaxJournalEntries
	}
	if o.MaxJournalBytes < journalHeaderBytes {
		return JournalOptions{}, ErrJournalLimit
	}
	if int64(o.Limits.MaxRecordBytes) > math.MaxUint32 {
		return JournalOptions{}, ErrJournalLimit
	}
	return o, nil
}

// Recovery describes repair performed while opening a journal.
type Recovery struct {
	TruncatedTail bool
	Floor         Version
	LastVersion   Version
	Entries       int
}

// Journal is an ordered, append-only lifecycle mutation log.
type Journal struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	limits     Limits
	maxBytes   int64
	maxEntries int
	size       int64
	floor      Version
	last       Version
	records    []Record
	closed     bool
}

// OpenJournal opens or creates a checksummed journal. A short final envelope is
// truncated and reported as a recoverable tail; complete corruption fails closed.
func OpenJournal(path string, options JournalOptions) (*Journal, Recovery, error) {
	if path == "" {
		return nil, Recovery{}, ErrJournalCorrupt
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, Recovery{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, Recovery{}, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, Recovery{}, err
	}
	if info.Size() > normalized.MaxJournalBytes {
		return nil, Recovery{}, ErrJournalLimit
	}
	if info.Size() == 0 {
		if err := writeFull(file, encodeJournalHeader(Version{})); err != nil {
			return nil, Recovery{}, err
		}
		if err := file.Sync(); err != nil {
			return nil, Recovery{}, err
		}
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			return nil, Recovery{}, err
		}
		journal := &Journal{
			path:       path,
			file:       file,
			limits:     normalized.Limits,
			maxBytes:   normalized.MaxJournalBytes,
			maxEntries: normalized.MaxJournalEntries,
			size:       journalHeaderBytes,
		}
		closeOnError = false
		return journal, Recovery{}, nil
	}

	data, err := readBounded(file, info.Size(), normalized.MaxJournalBytes)
	if err != nil {
		return nil, Recovery{}, err
	}
	floor, records, validSize, truncated, err := parseJournal(data, normalized.Limits)
	if err != nil {
		return nil, Recovery{}, err
	}
	if truncated {
		if err := file.Truncate(validSize); err != nil {
			return nil, Recovery{}, err
		}
		if err := file.Sync(); err != nil {
			return nil, Recovery{}, err
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, Recovery{}, err
	}
	last := floor
	if len(records) > 0 {
		last = records[len(records)-1].Version
	}
	if len(records) > normalized.MaxJournalEntries {
		return nil, Recovery{}, ErrJournalLimit
	}
	journal := &Journal{
		path:       path,
		file:       file,
		limits:     normalized.Limits,
		maxBytes:   normalized.MaxJournalBytes,
		maxEntries: normalized.MaxJournalEntries,
		size:       validSize,
		floor:      floor,
		last:       last,
		records:    records,
	}
	closeOnError = false
	return journal, Recovery{
		TruncatedTail: truncated,
		Floor:         floor,
		LastVersion:   last,
		Entries:       len(records),
	}, nil
}

// Append persists one strictly newer mutation and fsyncs it before returning.
func (j *Journal) Append(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Validate(j.limits); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("lifecycle: encode journal record: %w", err)
	}
	entry, err := encodeEnvelope(encoded)
	if err != nil {
		return err
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.ensureOpenLocked(); err != nil {
		return err
	}
	if record.Version.Compare(j.last) <= 0 {
		return ErrJournalVersionOrder
	}
	if len(j.records) >= j.maxEntries || j.size+int64(len(entry)) > j.maxBytes {
		return ErrJournalLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := j.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := writeFull(j.file, entry); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.records = append(j.records, record.clone())
	j.last = record.Version
	j.size += int64(len(entry))
	return nil
}

// Replay calls apply in durable journal order.
func (j *Journal) Replay(ctx context.Context, apply func(Record) error) error {
	if apply == nil {
		return ErrNilApply
	}
	records, err := j.Records(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := apply(record); err != nil {
			return err
		}
	}
	return nil
}

// Records returns the retained journal tail as owned copies.
func (j *Journal) Records(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.ensureOpenLocked(); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(j.records))
	for _, record := range j.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		records = append(records, record.clone())
	}
	return records, nil
}

// Close releases the journal file. Repeated closes are harmless.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

func (j *Journal) Floor() Version {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.floor
}

func (j *Journal) LastVersion() Version {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last
}

func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.records)
}

func (j *Journal) Bytes() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.size
}

func (j *Journal) ensureOpenLocked() error {
	if j.closed || j.file == nil {
		return ErrJournalClosed
	}
	return nil
}

// Checkpoint is a complete logical state at a journal watermark.
type Checkpoint struct {
	Watermark Version  `json:"watermark"`
	Records   []Record `json:"records"`
}

// SaveCheckpoint writes a validated checkpoint through a durable temp-file rename.
func SaveCheckpoint(ctx context.Context, path string, checkpoint Checkpoint, limits Limits) error {
	if path == "" {
		return ErrNilCheckpointPath
	}
	encoded, normalized, err := encodeCheckpoint(checkpoint, limits)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > int64(normalized.MaxCheckpointBytes) {
		return ErrCheckpointTooLarge
	}
	return atomicWrite(ctx, path, encoded)
}

// LoadCheckpoint reads and validates one checkpoint envelope.
func LoadCheckpoint(ctx context.Context, path string, limits Limits) (Checkpoint, error) {
	if path == "" {
		return Checkpoint{}, ErrNilCheckpointPath
	}
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	limits = limits.normalized()
	file, err := os.Open(path)
	if err != nil {
		return Checkpoint{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Checkpoint{}, err
	}
	if info.Size() > int64(limits.MaxCheckpointBytes) {
		return Checkpoint{}, ErrCheckpointTooLarge
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return Checkpoint{}, err
	}
	payload, next, err := decodeEnvelope(data, 0, limits.MaxCheckpointBytes, ErrCheckpointCorrupt, ErrCheckpointChecksum)
	if err != nil {
		return Checkpoint{}, err
	}
	if next != len(data) {
		return Checkpoint{}, ErrCheckpointCorrupt
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	normalized, _, err := normalizeCheckpoint(checkpoint, limits)
	if err != nil {
		return Checkpoint{}, err
	}
	return normalized, nil
}

// CompactionRequest describes a checkpoint and the peer acknowledgements that make
// truncation safe. Base is the checkpoint represented by the journal floor, if any.
type CompactionRequest struct {
	CheckpointPath string
	Watermark      Version
	Records        []Record
	Base           *Checkpoint
	PeerWatermarks []Version
}

// Compact durably writes the checkpoint before replacing the journal with its retained tail.
func (j *Journal) Compact(ctx context.Context, request CompactionRequest) error {
	if request.CheckpointPath == "" {
		return ErrNilCheckpointPath
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.ensureOpenLocked(); err != nil {
		return err
	}
	if filepath.Clean(request.CheckpointPath) == filepath.Clean(j.path) {
		return ErrCompactionUnsafe
	}
	if request.Watermark.Compare(j.floor) <= 0 {
		return ErrCompactionNoProgress
	}
	if request.Watermark.Compare(j.last) > 0 {
		return ErrCompactionWatermark
	}
	for _, peerWatermark := range request.PeerWatermarks {
		if peerWatermark.Compare(request.Watermark) < 0 {
			return ErrPeerBehindWatermark
		}
	}

	base, err := j.compactionBaseLocked(request.Base)
	if err != nil {
		return err
	}
	checkpoint, _, err := normalizeCheckpoint(Checkpoint{
		Watermark: request.Watermark,
		Records:   request.Records,
	}, j.limits)
	if err != nil {
		return err
	}
	state := NewStore(j.limits)
	for _, record := range base.Records {
		if _, err := state.Apply(record); err != nil {
			return fmt.Errorf("%w: base record: %v", ErrCompactionUnsafe, err)
		}
	}
	prefix := make([]Record, 0, len(j.records))
	for _, record := range j.records {
		if record.Version.Compare(request.Watermark) > 0 {
			break
		}
		prefix = append(prefix, record)
		if _, err := state.Apply(record); err != nil {
			return fmt.Errorf("%w: journal record: %v", ErrCompactionUnsafe, err)
		}
	}
	if len(prefix) == 0 {
		return ErrCompactionNoProgress
	}
	if !recordsEqual(state.Snapshot(), checkpoint.Records) {
		return ErrCompactionUnsafe
	}

	checkpointBytes, _, err := encodeCheckpoint(Checkpoint{
		Watermark: request.Watermark,
		Records:   checkpoint.Records,
	}, j.limits)
	if err != nil {
		return err
	}
	if err := atomicWrite(ctx, request.CheckpointPath, checkpointBytes); err != nil {
		return err
	}

	tail := make([]Record, 0, len(j.records)-len(prefix))
	if len(prefix) < len(j.records) {
		tail = append(tail, j.records[len(prefix):]...)
	}
	newFile, newSize, err := j.rewriteLocked(ctx, request.Watermark, tail)
	if err != nil {
		return err
	}
	oldFile := j.file
	if err := oldFile.Close(); err != nil {
		_ = newFile.Close()
		return err
	}
	j.file = newFile
	j.floor = request.Watermark
	j.records = cloneRecords(tail)
	j.size = newSize
	j.last = request.Watermark
	if len(j.records) > 0 {
		j.last = j.records[len(j.records)-1].Version
	}
	return nil
}

// InstallSnapshot durably installs a complete remote checkpoint and advances
// the journal floor. It is used for a peer that cannot be repaired from the
// retained journal tail; older local history is never retained as a suffix.
func (j *Journal) InstallSnapshot(ctx context.Context, checkpointPath string, checkpoint Checkpoint) error {
	if checkpointPath == "" {
		return ErrNilCheckpointPath
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.ensureOpenLocked(); err != nil {
		return err
	}
	if filepath.Clean(checkpointPath) == filepath.Clean(j.path) {
		return ErrCompactionUnsafe
	}
	normalized, _, err := normalizeCheckpoint(checkpoint, j.limits)
	if err != nil {
		return err
	}
	if normalized.Watermark.Compare(j.last) < 0 {
		return ErrSnapshotStale
	}
	if err := SaveCheckpoint(ctx, checkpointPath, normalized, j.limits); err != nil {
		return err
	}
	newFile, newSize, err := j.rewriteLocked(ctx, normalized.Watermark, nil)
	if err != nil {
		return err
	}
	oldFile := j.file
	if err := oldFile.Close(); err != nil {
		_ = newFile.Close()
		return err
	}
	j.file = newFile
	j.floor = normalized.Watermark
	j.last = normalized.Watermark
	j.records = nil
	j.size = newSize
	return nil
}

func (j *Journal) compactionBaseLocked(base *Checkpoint) (Checkpoint, error) {
	if j.floor.IsZero() {
		if base == nil {
			return Checkpoint{}, nil
		}
		validated, _, err := normalizeCheckpoint(*base, j.limits)
		if err != nil {
			return Checkpoint{}, err
		}
		if !validated.Watermark.IsZero() {
			return Checkpoint{}, ErrCompactionBaseMismatch
		}
		return validated, nil
	}
	if base == nil {
		return Checkpoint{}, ErrCompactionBaseMismatch
	}
	validated, _, err := normalizeCheckpoint(*base, j.limits)
	if err != nil {
		return Checkpoint{}, err
	}
	if validated.Watermark != j.floor {
		return Checkpoint{}, ErrCompactionBaseMismatch
	}
	return validated, nil
}

func (j *Journal) rewriteLocked(ctx context.Context, floor Version, records []Record) (*os.File, int64, error) {
	directory := filepath.Dir(j.path)
	tmp, err := os.CreateTemp(directory, ".streamhive-lifecycle-journal-*")
	if err != nil {
		return nil, 0, err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	header := encodeJournalHeader(floor)
	if err := writeFull(tmp, header); err != nil {
		_ = tmp.Close()
		return nil, 0, err
	}
	size := int64(len(header))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			_ = tmp.Close()
			return nil, 0, err
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			_ = tmp.Close()
			return nil, 0, err
		}
		entry, err := encodeEnvelope(encoded)
		if err != nil {
			_ = tmp.Close()
			return nil, 0, err
		}
		if size+int64(len(entry)) > j.maxBytes {
			_ = tmp.Close()
			return nil, 0, ErrJournalLimit
		}
		if err := writeFull(tmp, entry); err != nil {
			_ = tmp.Close()
			return nil, 0, err
		}
		size += int64(len(entry))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, 0, err
	}
	if err := tmp.Close(); err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(tmpName, j.path); err != nil {
		return nil, 0, err
	}
	removeTemp = false
	if err := syncDirectory(directory); err != nil {
		return nil, 0, err
	}
	newFile, err := os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if _, err := newFile.Seek(0, io.SeekEnd); err != nil {
		_ = newFile.Close()
		return nil, 0, err
	}
	return newFile, size, nil
}

type checkpointPayload = Checkpoint

func encodeCheckpoint(checkpoint Checkpoint, limits Limits) ([]byte, Limits, error) {
	limits = limits.normalized()
	normalized, _, err := normalizeCheckpoint(checkpoint, limits)
	if err != nil {
		return nil, Limits{}, err
	}
	payload, err := json.Marshal(checkpointPayload(normalized))
	if err != nil {
		return nil, Limits{}, fmt.Errorf("lifecycle: encode checkpoint: %w", err)
	}
	envelope, err := encodeEnvelope(payload)
	if err != nil {
		return nil, Limits{}, err
	}
	if len(envelope) > limits.MaxCheckpointBytes {
		return nil, Limits{}, ErrCheckpointTooLarge
	}
	return envelope, limits, nil
}

func normalizeCheckpoint(checkpoint Checkpoint, limits Limits) (Checkpoint, Limits, error) {
	limits = limits.normalized()
	records, err := normalizeCheckpointRecords(checkpoint.Records, checkpoint.Watermark, limits)
	if err != nil {
		return Checkpoint{}, Limits{}, err
	}
	return Checkpoint{Watermark: checkpoint.Watermark, Records: records}, limits, nil
}

func parseJournal(data []byte, limits Limits) (Version, []Record, int64, bool, error) {
	if len(data) < journalHeaderBytes {
		return Version{}, nil, 0, false, ErrJournalCorrupt
	}
	headerFloor, err := decodeJournalHeader(data[:journalHeaderBytes])
	if err != nil {
		return Version{}, nil, 0, false, err
	}
	records := make([]Record, 0)
	last := headerFloor
	offset := journalHeaderBytes
	for offset < len(data) {
		remaining := len(data) - offset
		if remaining < 4 {
			return headerFloor, records, int64(offset), true, nil
		}
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if length <= 0 || length > limits.MaxRecordBytes {
			return Version{}, nil, 0, false, ErrJournalCorrupt
		}
		total := 4 + length + 4
		if remaining < total {
			return headerFloor, records, int64(offset), true, nil
		}
		payload := data[offset+4 : offset+4+length]
		want := binary.BigEndian.Uint32(data[offset+4+length : offset+total])
		if crc32.Checksum(payload, crcTable) != want {
			return Version{}, nil, 0, false, ErrJournalChecksum
		}
		var record Record
		if err := json.Unmarshal(payload, &record); err != nil {
			return Version{}, nil, 0, false, fmt.Errorf("%w: record json: %v", ErrJournalCorrupt, err)
		}
		if err := record.Validate(limits); err != nil {
			return Version{}, nil, 0, false, fmt.Errorf("%w: record validation: %v", ErrJournalCorrupt, err)
		}
		if record.Version.Compare(last) <= 0 {
			return Version{}, nil, 0, false, ErrJournalVersionOrder
		}
		records = append(records, record.clone())
		last = record.Version
		offset += total
	}
	return headerFloor, records, int64(offset), false, nil
}

func encodeJournalHeader(floor Version) []byte {
	header := make([]byte, journalHeaderBytes)
	copy(header[:4], journalMagic)
	header[4] = journalFormat
	binary.BigEndian.PutUint64(header[8:16], floor.Epoch)
	binary.BigEndian.PutUint64(header[16:24], floor.Sequence)
	binary.BigEndian.PutUint32(header[24:28], crc32.Checksum(header[:24], crcTable))
	return header
}

func decodeJournalHeader(header []byte) (Version, error) {
	if len(header) != journalHeaderBytes || string(header[:4]) != journalMagic || header[4] != journalFormat {
		return Version{}, ErrJournalCorrupt
	}
	if crc32.Checksum(header[:24], crcTable) != binary.BigEndian.Uint32(header[24:28]) {
		return Version{}, ErrJournalChecksum
	}
	return Version{
		Epoch:    binary.BigEndian.Uint64(header[8:16]),
		Sequence: binary.BigEndian.Uint64(header[16:24]),
	}, nil
}

func encodeEnvelope(payload []byte) ([]byte, error) {
	if len(payload) == 0 || uint64(len(payload)) > math.MaxUint32 {
		return nil, ErrRecordTooLarge
	}
	envelope := make([]byte, 4+len(payload)+4)
	binary.BigEndian.PutUint32(envelope[:4], uint32(len(payload)))
	copy(envelope[4:], payload)
	binary.BigEndian.PutUint32(envelope[4+len(payload):], crc32.Checksum(payload, crcTable))
	return envelope, nil
}

func decodeEnvelope(data []byte, offset, max int, corrupt, checksum error) ([]byte, int, error) {
	if offset < 0 || offset > len(data) || len(data)-offset < 4 {
		return nil, 0, corrupt
	}
	length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
	if length <= 0 || length > max {
		return nil, 0, corrupt
	}
	total := 4 + length + 4
	if len(data)-offset < total {
		return nil, 0, corrupt
	}
	payload := data[offset+4 : offset+4+length]
	want := binary.BigEndian.Uint32(data[offset+4+length : offset+total])
	if crc32.Checksum(payload, crcTable) != want {
		return nil, 0, checksum
	}
	return payload, offset + total, nil
}

func readBounded(file *os.File, size, max int64) ([]byte, error) {
	if size > max {
		return nil, ErrJournalLimit
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrJournalLimit
	}
	return data, nil
}

func atomicWrite(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	tmp, err := os.CreateTemp(directory, ".streamhive-lifecycle-checkpoint-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := writeFull(tmp, data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTemp = false
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func cloneRecords(records []Record) []Record {
	out := make([]Record, 0, len(records))
	for _, record := range records {
		out = append(out, record.clone())
	}
	return out
}
