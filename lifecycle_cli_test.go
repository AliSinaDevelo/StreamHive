package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	require.NoError(t, sourceJournal.Append(ctx, record))
	require.NoError(t, sourceJournal.Close())

	var sourceOut, sourceErr, targetOut, targetErr safeBuffer
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- run(ctx, []string{
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

	targetDone := make(chan error, 1)
	go func() {
		targetDone <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-dial", sourceAddr,
			"-replicate",
			"-lifecycle",
			"-lifecycle-dir", targetDir,
			"-peer-auth-token", "shared-secret",
			"-peer-id", "target",
		}, &targetOut, &targetErr)
	}()

	targetJournalPath := filepath.Join(targetDir, "journal")
	require.Eventually(t, func() bool {
		journal, _, openErr := lifecycle.OpenJournal(targetJournalPath, lifecycle.JournalOptions{})
		if openErr != nil {
			return false
		}
		defer func() { _ = journal.Close() }()
		records, recordsErr := journal.Records(ctx)
		return recordsErr == nil && len(records) == 1 && reflect.DeepEqual(records[0], record)
	}, 8*time.Second, 20*time.Millisecond, "source stdout=%q stderr=%q target stdout=%q stderr=%q", sourceOut.String(), sourceErr.String(), targetOut.String(), targetErr.String())

	cancel()
	require.NoError(t, <-sourceDone)
	require.NoError(t, <-targetDone)
}
