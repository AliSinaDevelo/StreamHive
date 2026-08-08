package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
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

func TestRunLifecycleMutationFlagsAreStrict(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "requires opt in",
			args: []string{"-lifecycle-put-namespace", "demo", "-lifecycle-put-key", "item", "-lifecycle-put-data", "value"},
			want: "lifecycle: local mutation requires -lifecycle",
		},
		{
			name: "put and delete are exclusive",
			args: []string{"-lifecycle", "-lifecycle-put-namespace", "demo", "-lifecycle-put-key", "item", "-lifecycle-put-data", "value", "-lifecycle-delete-namespace", "demo", "-lifecycle-delete-key", "item"},
			want: "lifecycle: put and delete commands are mutually exclusive",
		},
		{
			name: "put requires namespace and key",
			args: []string{"-lifecycle", "-lifecycle-put-data", "value"},
			want: "lifecycle: put requires -lifecycle-put-namespace and -lifecycle-put-key",
		},
		{
			name: "delete requires namespace and key",
			args: []string{"-lifecycle", "-lifecycle-delete-namespace", "demo"},
			want: "lifecycle: delete requires -lifecycle-delete-namespace and -lifecycle-delete-key",
		},
		{
			name: "exit requires mutation",
			args: []string{"-lifecycle-exit-after-mutation"},
			want: "lifecycle: -lifecycle-exit-after-mutation requires a local mutation",
		},
		{
			name: "timeout is positive",
			args: []string{"-lifecycle-mutation-timeout", "0s"},
			want: "lifecycle: -lifecycle-mutation-timeout must be greater than zero",
		},
		{
			name: "blob key is hex sha256",
			args: []string{"-lifecycle", "-lifecycle-put-namespace", "demo", "-lifecycle-put-key", "item", "-lifecycle-put-data", "value", "-lifecycle-put-blob-key", "bad"},
			want: "lifecycle: invalid -lifecycle-put-blob-key",
		},
		{
			name: "compact requires opt in",
			args: []string{"-lifecycle-compact"},
			want: "lifecycle: -lifecycle-compact requires -lifecycle",
		},
		{
			name: "members require opt in",
			args: []string{"-lifecycle-members", "node-b"},
			want: "lifecycle: -lifecycle-members requires -lifecycle",
		},
		{
			name: "compact and mutation are exclusive",
			args: []string{"-lifecycle", "-lifecycle-compact", "-lifecycle-put-namespace", "demo", "-lifecycle-put-key", "item", "-lifecycle-put-data", "value"},
			want: "lifecycle: compaction cannot be combined with a local mutation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, io.Discard, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRunLifecycleCompactCommandWritesCheckpointAndExits(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	lifecycleDir := t.TempDir()
	baseArgs := []string{
		"-listen", "127.0.0.1:0",
		"-replicate",
		"-store-dir", storeDir,
		"-lifecycle",
		"-lifecycle-dir", lifecycleDir,
		"-peer-auth-token", "shared-secret",
		"-peer-id", "node-a",
		"-lifecycle-members=",
	}
	putArgs := append(append([]string(nil), baseArgs...),
		"-lifecycle-put-namespace", "demo",
		"-lifecycle-put-key", "item",
		"-lifecycle-put-data", "value",
		"-lifecycle-exit-after-mutation",
	)
	require.NoError(t, run(ctx, putArgs, io.Discard, io.Discard))

	var out safeBuffer
	compactArgs := append(append([]string(nil), baseArgs[:len(baseArgs)-1]...), "-lifecycle-compact")
	require.NoError(t, run(ctx, compactArgs, &out, io.Discard))
	assert.Contains(t, out.String(), "lifecycle compacted watermark=")
	assert.NotContains(t, out.String(), "listening on")

	journal, _, err := lifecycle.OpenJournal(filepath.Join(lifecycleDir, "journal"), lifecycle.JournalOptions{})
	require.NoError(t, err)
	assert.Equal(t, journal.LastVersion(), journal.Floor())
	assert.Equal(t, 0, journal.Len())
	require.NoError(t, journal.Close())
	checkpoint, err := lifecycle.LoadCheckpoint(ctx, filepath.Join(lifecycleDir, "checkpoint"), lifecycle.Limits{})
	require.NoError(t, err)
	require.Len(t, checkpoint.Records, 1)
	assert.Equal(t, lifecycle.StatePresent, checkpoint.Records[0].State)
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	data, err := store.Get(ctx, checkpoint.Records[0].BlobKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
}

func TestRunLifecyclePutCommitsAndExits(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	lifecycleDir := t.TempDir()
	var out safeBuffer
	err := run(ctx, []string{
		"-listen", "127.0.0.1:0",
		"-replicate",
		"-store-dir", storeDir,
		"-lifecycle",
		"-lifecycle-dir", lifecycleDir,
		"-peer-auth-token", "shared-secret",
		"-peer-id", "node-a",
		"-lifecycle-put-namespace", "demo",
		"-lifecycle-put-key", "item",
		"-lifecycle-put-data", "value",
		"-lifecycle-exit-after-mutation",
	}, &out, io.Discard)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "lifecycle mutation committed state=present")

	journal, _, err := lifecycle.OpenJournal(filepath.Join(lifecycleDir, "journal"), lifecycle.JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = journal.Close() }()
	records, err := journal.Records(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, lifecycle.StatePresent, records[0].State)
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	data, err := store.Get(ctx, records[0].BlobKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
}

func TestRunLifecycleMutationsResumeSequenceAndRetainBlobAfterDelete(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	lifecycleDir := t.TempDir()
	baseArgs := []string{
		"-listen", "127.0.0.1:0",
		"-replicate",
		"-store-dir", storeDir,
		"-lifecycle",
		"-lifecycle-dir", lifecycleDir,
		"-peer-auth-token", "shared-secret",
		"-peer-id", "node-a",
		"-lifecycle-exit-after-mutation",
	}
	putArgs := append(append([]string(nil), baseArgs...),
		"-lifecycle-put-namespace", "demo",
		"-lifecycle-put-key", "item",
		"-lifecycle-put-data", "value",
	)
	require.NoError(t, run(ctx, putArgs, io.Discard, io.Discard))
	deleteArgs := append(append([]string(nil), baseArgs...),
		"-lifecycle-delete-namespace", "demo",
		"-lifecycle-delete-key", "item",
	)
	require.NoError(t, run(ctx, deleteArgs, io.Discard, io.Discard))

	journal, _, err := lifecycle.OpenJournal(filepath.Join(lifecycleDir, "journal"), lifecycle.JournalOptions{})
	require.NoError(t, err)
	defer func() { _ = journal.Close() }()
	records, err := journal.Records(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, lifecycle.StatePresent, records[0].State)
	assert.Equal(t, lifecycle.StateDeleted, records[1].State)
	assert.Equal(t, records[0].Version.Epoch, records[1].Version.Epoch)
	assert.Equal(t, records[0].Version.Sequence+1, records[1].Version.Sequence)
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	data, err := store.Get(ctx, records[0].BlobKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
}

func TestRunLifecycleStatusRestoresAggregateStateAfterRestart(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	lifecycleDir := t.TempDir()
	store, err := storage.NewFileStore(storeDir)
	require.NoError(t, err)
	data := []byte("status value")
	blobKey := storage.SHA256Key(data)
	require.NoError(t, store.Put(ctx, blobKey, data))
	j, _, err := lifecycle.OpenJournal(filepath.Join(lifecycleDir, "journal"), lifecycle.JournalOptions{})
	require.NoError(t, err)
	records := []lifecycle.Record{
		{
			Namespace:   []byte("demo"),
			LogicalKey:  []byte("present-item"),
			State:       lifecycle.StatePresent,
			BlobKey:     blobKey,
			Version:     lifecycle.Version{Epoch: 2, Sequence: 1},
			AuthorityID: "node-a",
		},
		{
			Namespace:   []byte("demo"),
			LogicalKey:  []byte("deleted-item"),
			State:       lifecycle.StateDeleted,
			Version:     lifecycle.Version{Epoch: 2, Sequence: 2},
			AuthorityID: "node-a",
		},
	}
	for _, record := range records {
		require.NoError(t, j.Append(ctx, record))
	}
	require.NoError(t, j.Close())

	start := func() (context.CancelFunc, <-chan error, *safeBuffer, *safeBuffer, string) {
		nodeCtx, cancel := context.WithCancel(context.Background())
		var out, stderr safeBuffer
		done := make(chan error, 1)
		go func() {
			done <- run(nodeCtx, []string{
				"-listen", "127.0.0.1:0",
				"-health", "127.0.0.1:0",
				"-replicate",
				"-store-dir", storeDir,
				"-lifecycle",
				"-lifecycle-dir", lifecycleDir,
				"-peer-auth-token", "shared-secret",
				"-peer-id", "node-a",
			}, &out, &stderr)
		}()
		var health string
		healthPattern := regexp.MustCompile(`msg=health addr=([0-9a-fA-F.:]+)`)
		require.Eventually(t, func() bool {
			match := healthPattern.FindStringSubmatch(stderr.String())
			if len(match) != 2 {
				return false
			}
			health = match[1]
			return strings.Contains(out.String(), "listening on")
		}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())
		return cancel, done, &out, &stderr, health
	}

	readStatus := func(health string) lifecycleStatus {
		response, getErr := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + health + "/lifecycle/status")
		require.NoError(t, getErr)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var status lifecycleStatus
		require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
		return status
	}

	stop, done, out, stderr, health := start()
	status := readStatus(health)
	assert.True(t, status.Enabled)
	assert.True(t, status.Ready)
	assert.Equal(t, "ready", status.Readiness)
	assert.Equal(t, "node-a", status.AuthorityID)
	assert.Equal(t, lifecycle.Version{Epoch: 2, Sequence: 2}, status.AuthorityVersion)
	assert.Equal(t, lifecycle.Version{Epoch: 2, Sequence: 2}, status.JournalTail)
	assert.Equal(t, 2, status.JournalEntries)
	assert.Greater(t, status.JournalBytes, int64(0))
	assert.Equal(t, 2, status.LogicalRecords)
	assert.Equal(t, 1, status.Tombstones)
	assert.False(t, status.MembershipConfigured)
	assert.Equal(t, 0, status.MembershipMembers)
	assert.Equal(t, 0, status.MembershipAcknowledged)
	assert.Equal(t, lifecycle.Version{}, status.MembershipMinimum)
	assert.Equal(t, status.JournalTail, status.CompactionTarget)
	assert.True(t, status.CompactionBlocked)
	assert.Equal(t, "membership-missing", status.CompactionBlockedReason)
	assert.Zero(t, status.RepairSessionsActive)
	assert.Zero(t, status.RepairSessionsStarted)
	assert.Zero(t, status.RepairSessionsCompleted)
	assert.Zero(t, status.RepairSessionErrors)
	assert.Zero(t, status.RepairFramesReceived)
	assert.Zero(t, status.RepairFrameErrors)
	statusBody, err := json.Marshal(status)
	require.NoError(t, err)
	assert.NotContains(t, string(statusBody), "present-item")
	assert.NotContains(t, string(statusBody), "deleted-item")

	response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + health + "/metrics")
	require.NoError(t, err)
	var metrics map[string]int64
	require.NoError(t, json.NewDecoder(response.Body).Decode(&metrics))
	_ = response.Body.Close()
	assert.Equal(t, int64(1), metrics["lifecycle_ready"])
	assert.Equal(t, int64(2), metrics["lifecycle_authority_epoch"])
	assert.Equal(t, int64(2), metrics["lifecycle_authority_sequence"])
	assert.Equal(t, int64(2), metrics["lifecycle_journal_entries"])
	assert.Equal(t, int64(2), metrics["lifecycle_logical_records"])
	assert.Equal(t, int64(1), metrics["lifecycle_tombstones"])
	assert.Equal(t, int64(0), metrics["lifecycle_membership_configured"])
	assert.Equal(t, int64(0), metrics["lifecycle_membership_members"])
	assert.Equal(t, int64(0), metrics["lifecycle_membership_acknowledged"])
	assert.Equal(t, int64(0), metrics["lifecycle_membership_min_epoch"])
	assert.Equal(t, int64(0), metrics["lifecycle_membership_min_sequence"])
	assert.Equal(t, int64(2), metrics["lifecycle_compaction_target_epoch"])
	assert.Equal(t, int64(2), metrics["lifecycle_compaction_target_sequence"])
	assert.Equal(t, int64(1), metrics["lifecycle_compaction_blocked"])
	assert.Equal(t, int64(0), metrics["lifecycle_repair_session_errors"])
	assert.Equal(t, int64(0), metrics["lifecycle_repair_frames_received"])
	assert.Equal(t, int64(0), metrics["lifecycle_repair_frame_errors"])

	response, err = (&http.Client{Timeout: 2 * time.Second}).Get("http://" + health + "/metrics/prometheus")
	require.NoError(t, err)
	prometheus, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	_ = response.Body.Close()
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_journal_bytes gauge")
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_compaction_blocked gauge")
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_compaction_target_sequence gauge")
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_repair_session_errors counter")
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_repair_frames_received counter")
	assert.Contains(t, string(prometheus), "# TYPE streamhive_lifecycle_repair_frame_errors counter")
	assert.Contains(t, string(prometheus), "streamhive_lifecycle_ready 1")

	stop()
	require.NoError(t, <-done, "stdout=%q stderr=%q", out.String(), stderr.String())

	restartedStop, restartedDone, restartedOut, restartedErr, restartedHealth := start()
	restartedStatus := readStatus(restartedHealth)
	assert.Equal(t, status.AuthorityID, restartedStatus.AuthorityID)
	assert.Equal(t, status.AuthorityVersion, restartedStatus.AuthorityVersion)
	assert.Equal(t, status.JournalFloor, restartedStatus.JournalFloor)
	assert.Equal(t, status.JournalTail, restartedStatus.JournalTail)
	assert.Equal(t, status.LogicalRecords, restartedStatus.LogicalRecords)
	restartedStop()
	require.NoError(t, <-restartedDone, "stdout=%q stderr=%q", restartedOut.String(), restartedErr.String())
}

func TestLifecycleRuntimeStatusExposesRepairOutcomeCounters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runtime, err := openLifecycleRuntime(ctx, testLifecycleCLIConfig(dir), storage.NewMemoryStore(), "token", "node-a")
	require.NoError(t, err)
	defer func() { _ = runtime.Close() }()

	runtime.metrics.SessionsStarted.Store(4)
	runtime.metrics.SessionsCompleted.Store(2)
	runtime.metrics.SessionsActive.Store(1)
	runtime.metrics.SessionErrors.Store(3)
	runtime.metrics.FramesReceived.Store(8)
	runtime.metrics.FrameErrors.Store(2)

	status := runtime.Status()
	assert.Equal(t, int64(1), status.RepairSessionsActive)
	assert.Equal(t, uint64(4), status.RepairSessionsStarted)
	assert.Equal(t, uint64(2), status.RepairSessionsCompleted)
	assert.Equal(t, uint64(3), status.RepairSessionErrors)
	assert.Equal(t, uint64(8), status.RepairFramesReceived)
	assert.Equal(t, uint64(2), status.RepairFrameErrors)

	metrics := runtime.Metrics()
	assert.Equal(t, int64(3), metrics["lifecycle_repair_session_errors"])
	assert.Equal(t, int64(8), metrics["lifecycle_repair_frames_received"])
	assert.Equal(t, int64(2), metrics["lifecycle_repair_frame_errors"])
	statusBody, err := json.Marshal(status)
	require.NoError(t, err)
	assert.NotContains(t, string(statusBody), "peer-b")
	assert.NotContains(t, string(statusBody), "secret-key")
}

func TestRunLifecycleMutationConvergesOverAuthenticatedTCP(t *testing.T) {
	targetCtx, targetCancel := context.WithCancel(context.Background())
	defer targetCancel()
	targetStoreDir := t.TempDir()
	targetLifecycleDir := t.TempDir()
	var targetOut, targetErr safeBuffer
	targetDone := make(chan error, 1)
	go func() {
		targetDone <- run(targetCtx, []string{
			"-listen", "127.0.0.1:0",
			"-replicate",
			"-store-dir", targetStoreDir,
			"-lifecycle",
			"-lifecycle-dir", targetLifecycleDir,
			"-peer-auth-token", "shared-secret",
			"-peer-id", "target",
			"-peer-allow-ids", "source",
		}, &targetOut, &targetErr)
	}()

	listenPattern := regexp.MustCompile(`listening on ([^\n]+)`)
	var targetAddr string
	require.Eventually(t, func() bool {
		match := listenPattern.FindStringSubmatch(targetOut.String())
		if len(match) != 2 {
			return false
		}
		targetAddr = strings.TrimSpace(match[1])
		return targetAddr != ""
	}, 3*time.Second, 10*time.Millisecond, "target stdout=%q stderr=%q", targetOut.String(), targetErr.String())

	sourceStoreDir := t.TempDir()
	sourceLifecycleDir := t.TempDir()
	var sourceOut, sourceErr safeBuffer
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- run(context.Background(), []string{
			"-listen", "127.0.0.1:0",
			"-dial", targetAddr,
			"-replicate",
			"-store-dir", sourceStoreDir,
			"-lifecycle",
			"-lifecycle-dir", sourceLifecycleDir,
			"-peer-auth-token", "shared-secret",
			"-peer-id", "source",
			"-lifecycle-put-namespace", "demo",
			"-lifecycle-put-key", "item",
			"-lifecycle-put-data", "value",
			"-lifecycle-exit-after-mutation",
			"-lifecycle-mutation-timeout", "8s",
		}, &sourceOut, &sourceErr)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(sourceOut.String(), "lifecycle mutation committed state=present")
	}, 3*time.Second, 10*time.Millisecond, "source stdout=%q stderr=%q", sourceOut.String(), sourceErr.String())
	require.NoError(t, <-sourceDone, "source stdout=%q stderr=%q", sourceOut.String(), sourceErr.String())

	targetJournalPath := filepath.Join(targetLifecycleDir, "journal")
	require.Eventually(t, func() bool {
		journal, _, openErr := lifecycle.OpenJournal(targetJournalPath, lifecycle.JournalOptions{})
		if openErr != nil {
			return false
		}
		defer func() { _ = journal.Close() }()
		records, recordsErr := journal.Records(context.Background())
		if recordsErr != nil || len(records) != 1 {
			return false
		}
		if records[0].State != lifecycle.StatePresent || string(records[0].Namespace) != "demo" || string(records[0].LogicalKey) != "item" {
			return false
		}
		store, storeErr := storage.NewFileStore(targetStoreDir)
		if storeErr != nil {
			return false
		}
		data, getErr := store.Get(context.Background(), records[0].BlobKey)
		return getErr == nil && string(data) == "value"
	}, 8*time.Second, 20*time.Millisecond, "target stdout=%q stderr=%q source stdout=%q stderr=%q", targetOut.String(), targetErr.String(), sourceOut.String(), sourceErr.String())

	targetCancel()
	require.NoError(t, <-targetDone, "target stdout=%q stderr=%q", targetOut.String(), targetErr.String())
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

func TestLifecycleRuntimeCompactionRequiresExplicitMembershipFence(t *testing.T) {
	ctx := context.Background()

	missingDir := t.TempDir()
	missing, err := openLifecycleRuntime(ctx, testLifecycleCLIConfig(missingDir), storage.NewMemoryStore(), "token", "node-a")
	require.NoError(t, err)
	_, _, err = missing.put(ctx, "demo", "item", []byte("value"), nil)
	require.NoError(t, err)
	assert.ErrorIs(t, missing.compact(ctx), lifecycle.ErrMembershipNotConfigured)
	assert.Equal(t, lifecycle.Version{}, missing.journal.Floor())
	require.NoError(t, missing.Close())

	behindDir := t.TempDir()
	behindConfig := testLifecycleCLIConfig(behindDir)
	behindConfig.membershipConfigured = true
	behindConfig.membershipMembers = []string{"peer-b"}
	behind, err := openLifecycleRuntime(ctx, behindConfig, storage.NewMemoryStore(), "token", "node-a")
	require.NoError(t, err)
	_, _, err = behind.put(ctx, "demo", "item", []byte("value"), nil)
	require.NoError(t, err)
	behindStatus := behind.Status()
	assert.Equal(t, 1, behindStatus.MembershipMembers)
	assert.Equal(t, 0, behindStatus.MembershipAcknowledged)
	assert.True(t, behindStatus.MembershipMinimum.IsZero())
	assert.Equal(t, behindStatus.JournalTail, behindStatus.CompactionTarget)
	assert.Equal(t, "member-behind", behindStatus.CompactionBlockedReason)
	assert.ErrorIs(t, behind.compact(ctx), lifecycle.ErrMembershipBehind)
	assert.Equal(t, lifecycle.Version{}, behind.journal.Floor())
	require.NoError(t, behind.Close())

	emptyDir := t.TempDir()
	emptyConfig := testLifecycleCLIConfig(emptyDir)
	emptyConfig.membershipConfigured = true
	emptyConfig.membershipMembers = []string{}
	empty, err := openLifecycleRuntime(ctx, emptyConfig, storage.NewMemoryStore(), "token", "node-a")
	require.NoError(t, err)
	deleted, _, err := empty.delete(ctx, "demo", "item")
	require.NoError(t, err)
	require.NoError(t, empty.compact(ctx))
	assert.Equal(t, deleted.Version, empty.journal.Floor())
	remaining, err := empty.journal.Records(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining)
	assert.True(t, empty.membership.Configured())
	require.NoError(t, empty.Close())
}

func TestLifecycleRuntimeCompactionRestoresCheckpointAndRefusesCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := storage.NewMemoryStore()
	config := testLifecycleCLIConfig(dir)
	config.membershipConfigured = true
	config.membershipMembers = []string{"peer-b"}
	runtime, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	putRecord, _, err := runtime.put(ctx, "demo", "item", []byte("value"), nil)
	require.NoError(t, err)
	deletedRecord, _, err := runtime.delete(ctx, "demo", "item")
	require.NoError(t, err)
	require.NoError(t, runtime.coordinator.Acknowledge(ctx, "peer-b", deletedRecord.Version))
	readyToCompact := runtime.Status()
	assert.Equal(t, 1, readyToCompact.MembershipMembers)
	assert.Equal(t, 1, readyToCompact.MembershipAcknowledged)
	assert.Equal(t, deletedRecord.Version, readyToCompact.MembershipMinimum)
	assert.False(t, readyToCompact.CompactionBlocked)
	require.NoError(t, runtime.compact(ctx))
	assert.Equal(t, deletedRecord.Version, runtime.journal.Floor())
	assert.Equal(t, deletedRecord.Version, runtime.journal.LastVersion())
	assert.Equal(t, 0, runtime.journal.Len())
	checkpoint, err := lifecycle.LoadCheckpoint(ctx, filepath.Join(dir, "checkpoint"), lifecycle.Limits{})
	require.NoError(t, err)
	assert.Equal(t, deletedRecord.Version, checkpoint.Watermark)
	assert.Equal(t, []lifecycle.Record{deletedRecord}, checkpoint.Records)
	data, err := store.Get(ctx, putRecord.BlobKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("value"), data)
	require.NoError(t, runtime.Close())

	restarted, err := openLifecycleRuntime(ctx, config, store, "token", "node-a")
	require.NoError(t, err)
	assert.Equal(t, deletedRecord.Version, restarted.journal.Floor())
	assert.Equal(t, deletedRecord.Version, restarted.journal.LastVersion())
	assert.Equal(t, 0, restarted.journal.Len())
	assert.True(t, restarted.membership.Configured())
	got, ok, err := restarted.state.Get([]byte("demo"), []byte("item"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, deletedRecord, got)
	require.NoError(t, restarted.Close())

	checkpointBytes, err := os.ReadFile(filepath.Join(dir, "checkpoint"))
	require.NoError(t, err)
	checkpointBytes[len(checkpointBytes)-1] ^= 0xff
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint"), checkpointBytes, 0o600))
	_, err = openLifecycleRuntime(ctx, config, store, "token", "node-a")
	assert.ErrorIs(t, err, lifecycle.ErrCheckpointChecksum)
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

func TestRunLifecycleSnapshotRepairsStalePeerAfterCompaction(t *testing.T) {
	ctx := context.Background()
	sourceStoreDir := t.TempDir()
	sourceLifecycleDir := t.TempDir()
	targetStoreDir := t.TempDir()
	targetLifecycleDir := t.TempDir()
	const token = "shared-secret"
	liveData := []byte("live-value")
	liveBlobKey := storage.SHA256Key(liveData)

	mutationArgs := []string{
		"-listen", "127.0.0.1:0",
		"-replicate",
		"-store-dir", sourceStoreDir,
		"-lifecycle",
		"-lifecycle-dir", sourceLifecycleDir,
		"-peer-auth-token", token,
		"-peer-id", "source",
		"-lifecycle-members", "target",
		"-lifecycle-exit-after-mutation",
	}
	putLive := append(append([]string(nil), mutationArgs...),
		"-lifecycle-put-namespace", "demo",
		"-lifecycle-put-key", "live",
		"-lifecycle-put-data", string(liveData),
	)
	require.NoError(t, run(ctx, putLive, io.Discard, io.Discard))
	putRetired := append(append([]string(nil), mutationArgs...),
		"-lifecycle-put-namespace", "demo",
		"-lifecycle-put-key", "retired",
		"-lifecycle-put-data", "retired-value",
	)
	require.NoError(t, run(ctx, putRetired, io.Discard, io.Discard))
	deleteRetired := append(append([]string(nil), mutationArgs...),
		"-lifecycle-delete-namespace", "demo",
		"-lifecycle-delete-key", "retired",
	)
	require.NoError(t, run(ctx, deleteRetired, io.Discard, io.Discard))

	listenPattern := regexp.MustCompile(`listening on ([^\n]+)`)
	startNode := func(nodeCtx context.Context, args []string) (context.CancelFunc, <-chan error, *safeBuffer, *safeBuffer, string) {
		nodeCtx, nodeCancel := context.WithCancel(nodeCtx)
		var out, stderr safeBuffer
		done := make(chan error, 1)
		go func() {
			done <- run(nodeCtx, args, &out, &stderr)
		}()
		var address string
		require.Eventually(t, func() bool {
			match := listenPattern.FindStringSubmatch(out.String())
			if len(match) != 2 {
				return false
			}
			address = strings.TrimSpace(match[1])
			return address != ""
		}, 3*time.Second, 10*time.Millisecond, "stdout=%q stderr=%q", out.String(), stderr.String())
		return nodeCancel, done, &out, &stderr, address
	}

	sourceArgs := []string{
		"-listen", "127.0.0.1:0",
		"-replicate",
		"-store-dir", sourceStoreDir,
		"-lifecycle",
		"-lifecycle-dir", sourceLifecycleDir,
		"-peer-auth-token", token,
		"-peer-id", "source",
	}
	sourceCancel, sourceDone, sourceOut, sourceErr, sourceAddr := startNode(context.Background(), sourceArgs)
	targetArgs := []string{
		"-listen", "127.0.0.1:0",
		"-dial", sourceAddr,
		"-replicate",
		"-store-dir", targetStoreDir,
		"-lifecycle",
		"-lifecycle-dir", targetLifecycleDir,
		"-peer-auth-token", token,
		"-peer-id", "target",
	}
	targetCancel, targetDone, targetOut, targetErr, _ := startNode(context.Background(), targetArgs)

	readJournal := func(path string) ([]lifecycle.Record, lifecycle.Version, bool) {
		journal, _, err := lifecycle.OpenJournal(path, lifecycle.JournalOptions{})
		if err != nil {
			return nil, lifecycle.Version{}, false
		}
		defer func() { _ = journal.Close() }()
		records, err := journal.Records(ctx)
		return records, journal.LastVersion(), err == nil
	}
	sourceJournalPath := filepath.Join(sourceLifecycleDir, "journal")
	targetJournalPath := filepath.Join(targetLifecycleDir, "journal")
	var sourceTail lifecycle.Version
	require.Eventually(t, func() bool {
		records, tail, ok := readJournal(sourceJournalPath)
		if !ok || len(records) != 3 {
			return false
		}
		sourceTail = tail
		targetRecords, _, targetOK := readJournal(targetJournalPath)
		return targetOK && len(targetRecords) == 3
	}, 8*time.Second, 20*time.Millisecond, "source stdout=%q stderr=%q target stdout=%q stderr=%q", sourceOut.String(), sourceErr.String(), targetOut.String(), targetErr.String())
	require.Eventually(t, func() bool {
		book, err := lifecycle.OpenWatermarkBook(filepath.Join(sourceLifecycleDir, "watermarks"), lifecycle.WatermarkOptions{})
		return err == nil && book.Watermark("target") == sourceTail
	}, 8*time.Second, 20*time.Millisecond, "source stdout=%q stderr=%q target stdout=%q stderr=%q", sourceOut.String(), sourceErr.String(), targetOut.String(), targetErr.String())

	targetCancel()
	require.NoError(t, <-targetDone, "target stdout=%q stderr=%q", targetOut.String(), targetErr.String())
	sourceCancel()
	require.NoError(t, <-sourceDone, "source stdout=%q stderr=%q", sourceOut.String(), sourceErr.String())

	var compactOut safeBuffer
	require.NoError(t, run(ctx, []string{
		"-replicate",
		"-store-dir", sourceStoreDir,
		"-lifecycle",
		"-lifecycle-dir", sourceLifecycleDir,
		"-peer-auth-token", token,
		"-peer-id", "source",
		"-lifecycle-compact",
	}, &compactOut, io.Discard))
	assert.Contains(t, compactOut.String(), "lifecycle compacted watermark=")
	checkpoint, err := lifecycle.LoadCheckpoint(ctx, filepath.Join(sourceLifecycleDir, "checkpoint"), lifecycle.Limits{})
	require.NoError(t, err)
	assert.Equal(t, sourceTail, checkpoint.Watermark)
	require.Len(t, checkpoint.Records, 2)

	targetStore, err := storage.NewFileStore(targetStoreDir)
	require.NoError(t, err)
	require.NoError(t, targetStore.Delete(ctx, liveBlobKey))
	require.NoError(t, os.RemoveAll(targetLifecycleDir))
	require.NoError(t, os.MkdirAll(targetLifecycleDir, 0o700))

	sourceCancel, sourceDone, sourceOut, sourceErr, sourceAddr = startNode(context.Background(), sourceArgs)
	targetArgs[3] = sourceAddr
	targetCancel, targetDone, targetOut, targetErr, _ = startNode(context.Background(), targetArgs)
	require.Eventually(t, func() bool {
		got, getErr := targetStore.Get(ctx, liveBlobKey)
		if getErr != nil || !reflect.DeepEqual(got, liveData) {
			return false
		}
		targetCheckpoint, checkpointErr := lifecycle.LoadCheckpoint(ctx, filepath.Join(targetLifecycleDir, "checkpoint"), lifecycle.Limits{})
		if checkpointErr != nil || targetCheckpoint.Watermark != sourceTail || len(targetCheckpoint.Records) != 2 {
			return false
		}
		targetJournal, _, journalErr := lifecycle.OpenJournal(targetJournalPath, lifecycle.JournalOptions{})
		if journalErr != nil {
			return false
		}
		defer func() { _ = targetJournal.Close() }()
		return targetJournal.Floor() == sourceTail && targetJournal.Len() == 0
	}, 30*time.Second, 20*time.Millisecond, "source stdout=%q stderr=%q target stdout=%q stderr=%q", sourceOut.String(), sourceErr.String(), targetOut.String(), targetErr.String())
	targetCheckpoint, err := lifecycle.LoadCheckpoint(ctx, filepath.Join(targetLifecycleDir, "checkpoint"), lifecycle.Limits{})
	require.NoError(t, err)
	var liveRecord, retiredRecord lifecycle.Record
	for _, record := range targetCheckpoint.Records {
		switch string(record.LogicalKey) {
		case "live":
			liveRecord = record
		case "retired":
			retiredRecord = record
		}
	}
	assert.Equal(t, lifecycle.StatePresent, liveRecord.State)
	assert.Equal(t, liveBlobKey, liveRecord.BlobKey)
	assert.Equal(t, lifecycle.StateDeleted, retiredRecord.State)

	targetCancel()
	require.NoError(t, <-targetDone, "target stdout=%q stderr=%q", targetOut.String(), targetErr.String())
	sourceCancel()
	require.NoError(t, <-sourceDone, "source stdout=%q stderr=%q", sourceOut.String(), sourceErr.String())
}
