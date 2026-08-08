package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/internal/lifecycle"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLifecycleCLIConfig(dir string) lifecycleCLIConfig {
	return lifecycleCLIConfig{
		enabled: true,
		dir:     dir,
		repairLimits: lifecycle.RepairLimits{
			MaxRecords:         2,
			MaxLogicalKeyBytes: 1024,
			MaxMetadataBytes:   4096,
			MaxFrameBytes:      8192,
		},
	}
}

func TestLifecycleCLIConfigRequiresExplicitRuntimeDependencies(t *testing.T) {
	store := storage.NewMemoryStore()

	assert.EqualError(t,
		(lifecycleCLIConfig{dir: t.TempDir()}).validate(nil, "", ""),
		"lifecycle: -lifecycle-dir requires -lifecycle",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(nil, "token", "node-a"),
		"lifecycle: -lifecycle requires -replicate",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(store, "", "node-a"),
		"lifecycle: -lifecycle requires -peer-auth-token",
	)
	assert.EqualError(t,
		testLifecycleCLIConfig(t.TempDir()).validate(store, "token", ""),
		"lifecycle: -lifecycle requires -peer-id",
	)
	assert.NoError(t, testLifecycleCLIConfig(t.TempDir()).validate(store, "token", "node-a"))
}

func TestOpenLifecycleRuntimeRestoresDurableState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := storage.NewMemoryStore()
	config := testLifecycleCLIConfig(dir)
	runtime, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	record := lifecycle.Record{
		Namespace:   []byte("demo"),
		LogicalKey:  []byte("item"),
		State:       lifecycle.StateDeleted,
		Version:     lifecycle.Version{Epoch: 1, Sequence: 1},
		AuthorityID: "node-a",
	}
	_, err = runtime.applier.Apply(ctx, []string{lifecycle.LifecycleCapabilityV1}, record, nil)
	require.NoError(t, err)
	require.NoError(t, runtime.coordinator.Acknowledge(ctx, "peer-b", record.Version))
	require.NoError(t, runtime.Close())

	restarted, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = restarted.Close() }()
	got, ok, err := restarted.state.Get(record.Namespace, record.LogicalKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record, got)
	assert.FileExists(t, filepath.Join(dir, "journal"))
	assert.FileExists(t, filepath.Join(dir, "watermarks"))
	assert.FileExists(t, filepath.Join(dir, "authority"))
}

func TestOpenLifecycleRuntimeResumesAuthoritySequence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config := testLifecycleCLIConfig(dir)
	store := storage.NewMemoryStore()

	runtime, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	first, err := runtime.nextVersion(ctx)
	require.NoError(t, err)
	current := runtime.authority.Current()
	require.NoError(t, runtime.Close())

	restarted, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = restarted.Close() }()
	assert.Equal(t, current, restarted.authority.Current())
	second, err := restarted.nextVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.Epoch, second.Epoch)
	assert.Equal(t, first.Sequence+1, second.Sequence)
}

func TestLifecycleRuntimePutDeletePreservesRawBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &trackingDeleteStore{BlobStore: storage.NewMemoryStore()}
	runtime, err := openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), store, "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = runtime.Close() }()

	data := []byte("local lifecycle value")
	putRecord, putResult, err := runtime.put(ctx, "demo", "item", data, nil)
	require.NoError(t, err)
	assert.Equal(t, lifecycle.OutcomeApplied, putResult.Outcome)

	deletedRecord, deleteResult, err := runtime.delete(ctx, "demo", "item")
	require.NoError(t, err)
	assert.Equal(t, lifecycle.OutcomeApplied, deleteResult.Outcome)
	assert.Equal(t, putRecord.Version.Epoch, deletedRecord.Version.Epoch)
	assert.Equal(t, putRecord.Version.Sequence+1, deletedRecord.Version.Sequence)
	assert.Equal(t, 2, runtime.journal.Len())
	assert.Equal(t, int32(0), store.deletes.Load())
	stored, err := store.Get(ctx, putRecord.BlobKey)
	require.NoError(t, err)
	assert.Equal(t, data, stored)
	current, ok, err := runtime.state.Get([]byte("demo"), []byte("item"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, deletedRecord, current)

	metrics := runtime.Metrics()
	assert.Equal(t, int64(2), metrics["lifecycle_mutations_started"])
	assert.Equal(t, int64(2), metrics["lifecycle_mutations_applied"])
}

func TestLifecycleRuntimeRejectsCorruptSuppliedBlobKeyWithoutRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runtime, err := openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), storage.NewMemoryStore(), "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = runtime.Close() }()

	_, _, err = runtime.put(ctx, "demo", "item", []byte("value"), []byte("wrong"))
	assert.ErrorIs(t, err, storage.ErrInvalidSHA256Key)
	assert.Equal(t, 0, runtime.journal.Len())
	_, ok, getErr := runtime.state.Get([]byte("demo"), []byte("item"))
	require.NoError(t, getErr)
	assert.False(t, ok)
}

func TestLifecycleRuntimeRawWriteFailureDoesNotPublishRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &failingPutStore{BlobStore: storage.NewMemoryStore(), err: errors.New("disk full")}
	runtime, err := openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), store, "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = runtime.Close() }()
	before := runtime.authority.Current()

	_, _, err = runtime.put(ctx, "demo", "item", []byte("value"), nil)
	assert.EqualError(t, err, "disk full")
	assert.Equal(t, 0, runtime.journal.Len())
	_, ok, getErr := runtime.state.Get([]byte("demo"), []byte("item"))
	require.NoError(t, getErr)
	assert.False(t, ok)
	assert.True(t, runtime.authority.Current().Compare(before) > 0)
}

type trackingDeleteStore struct {
	storage.BlobStore
	deletes atomic.Int32
}

func (s *trackingDeleteStore) Delete(ctx context.Context, key []byte) error {
	s.deletes.Add(1)
	return s.BlobStore.Delete(ctx, key)
}

type failingPutStore struct {
	storage.BlobStore
	err error
}

func (s *failingPutStore) Put(context.Context, []byte, []byte) error {
	return s.err
}

func TestOpenLifecycleRuntimeRefusesMissingRestoredBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "journal")
	journal, _, err := lifecycle.OpenJournal(journalPath, lifecycle.JournalOptions{})
	require.NoError(t, err)
	record := lifecycle.Record{
		Namespace:   []byte("demo"),
		LogicalKey:  []byte("item"),
		State:       lifecycle.StatePresent,
		BlobKey:     storage.SHA256Key([]byte("missing")),
		Version:     lifecycle.Version{Epoch: 1, Sequence: 1},
		AuthorityID: "node-a",
	}
	require.NoError(t, journal.Append(ctx, record))
	require.NoError(t, journal.Close())

	_, err = openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), storage.NewMemoryStore(), "token", "node-a")
	require.Error(t, err)
	assert.True(t, errors.Is(err, lifecycle.ErrLifecycleBlobMissing))
}

func TestRunLifecycleRepairConvergesOverAuthenticatedTCP(t *testing.T) {
	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	defer sourceCancel()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	record := lifecycle.Record{
		Namespace:   []byte("demo"),
		LogicalKey:  []byte("item"),
		State:       lifecycle.StateDeleted,
		Version:     lifecycle.Version{Epoch: 1, Sequence: 1},
		AuthorityID: "source",
	}
	sourceJournal, _, err := lifecycle.OpenJournal(filepath.Join(sourceDir, "journal"), lifecycle.JournalOptions{})
	require.NoError(t, err)
	require.NoError(t, sourceJournal.Append(context.Background(), record))
	require.NoError(t, sourceJournal.Close())

	var sourceOut, sourceErr, targetOut, targetErr safeBuffer
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- run(sourceCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-lifecycle",
			"-lifecycle-dir", sourceDir,
			"-peer-auth-token", "shared-secret",
			"-peer-id", "source",
		}, &sourceOut, &sourceErr)
	}()

	listenPattern := regexp.MustCompile(`listening on ([^\n]+)`)
	var sourceAddr string
	require.Eventually(t, func() bool {
		match := listenPattern.FindStringSubmatch(sourceOut.String())
		if len(match) != 2 {
			return false
		}
		sourceAddr = strings.TrimSpace(match[1])
		return sourceAddr != ""
	}, 3*time.Second, 10*time.Millisecond, "source stdout=%q stderr=%q", sourceOut.String(), sourceErr.String())

	startTarget := func(ctx context.Context, out, stderr *safeBuffer) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{
				"-listen", "127.0.0.1:0",
				"-dial", sourceAddr,
				"-replicate",
				"-lifecycle",
				"-lifecycle-dir", targetDir,
				"-peer-auth-token", "shared-secret",
				"-peer-id", "target",
			}, out, stderr)
		}()
		return done
	}
	targetCtx, targetCancel := context.WithCancel(context.Background())
	targetDone := startTarget(targetCtx, &targetOut, &targetErr)

	targetJournalPath := filepath.Join(targetDir, "journal")
	require.Eventually(t, func() bool {
		journal, _, openErr := lifecycle.OpenJournal(targetJournalPath, lifecycle.JournalOptions{})
		if openErr != nil {
			return false
		}
		defer func() { _ = journal.Close() }()
		records, recordsErr := journal.Records(context.Background())
		return recordsErr == nil && len(records) == 1 && reflect.DeepEqual(records[0], record)
	}, 8*time.Second, 20*time.Millisecond, "source stdout=%q stderr=%q target stdout=%q stderr=%q", sourceOut.String(), sourceErr.String(), targetOut.String(), targetErr.String())

	targetCancel()
	require.NoError(t, <-targetDone)

	restartedTargetCtx, restartedTargetCancel := context.WithCancel(context.Background())
	var restartedTargetOut, restartedTargetErr safeBuffer
	restartedTargetDone := startTarget(restartedTargetCtx, &restartedTargetOut, &restartedTargetErr)
	require.Eventually(t, func() bool {
		return strings.Contains(restartedTargetOut.String(), "listening on")
	}, 3*time.Second, 10*time.Millisecond, "restarted target stdout=%q stderr=%q", restartedTargetOut.String(), restartedTargetErr.String())
	require.Eventually(t, func() bool {
		journal, _, openErr := lifecycle.OpenJournal(targetJournalPath, lifecycle.JournalOptions{})
		if openErr != nil {
			return false
		}
		defer func() { _ = journal.Close() }()
		records, recordsErr := journal.Records(context.Background())
		return recordsErr == nil && len(records) == 1 && reflect.DeepEqual(records[0], record)
	}, 5*time.Second, 20*time.Millisecond, "restarted target stdout=%q stderr=%q", restartedTargetOut.String(), restartedTargetErr.String())
	restartedTargetCancel()
	require.NoError(t, <-restartedTargetDone)
	sourceCancel()
	require.NoError(t, <-sourceDone)
}
