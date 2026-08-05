package main

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairContinuationSchedulerSaturatesAtConfiguredKeyBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	maxKeys := replication.DefaultMaxKeys
	metrics := &replicationMetrics{}
	peer := &asyncCapturePeer{}
	scheduler := newRepairContinuationScheduler(
		ctx,
		storage.NewMemoryStore(),
		replication.Limits{MaxKeys: maxKeys},
		0,
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Hour,
	)

	keys := make([][]byte, maxKeys+1)
	for i := range keys {
		keys[i] = []byte("budget-" + strconv.Itoa(i))
	}
	scheduler.Schedule(peer, keys)

	assert.Equal(t, int64(1), metrics.RepairContinuationsActive.Load())
	assert.Equal(t, int64(maxKeys), metrics.RepairContinuationKeysPending.Load())
	assert.Equal(t, uint64(1), metrics.RepairContinuationsDropped.Load())

	scheduler.mu.Lock()
	entry := scheduler.entries[repairPeerKey(peer)]
	require.NotNil(t, entry)
	assert.Len(t, entry.pending, maxKeys)
	scheduler.mu.Unlock()

	t.Logf(
		"queue saturation: max_keys=%d requested=%d admitted=%d dropped_events=%d active=%d pending=%d",
		maxKeys,
		len(keys),
		len(keys)-1,
		metrics.RepairContinuationsDropped.Load(),
		metrics.RepairContinuationsActive.Load(),
		metrics.RepairContinuationKeysPending.Load(),
	)

	scheduler.Forget(peer)
	assert.Zero(t, metrics.RepairContinuationsActive.Load())
	assert.Zero(t, metrics.RepairContinuationKeysPending.Load())
}

func TestRepairIOLimiterKeepsHealthyPeerProgressing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics := &replicationMetrics{}
	metrics.repairBudget = newRepairIOLimiter(1, metrics)
	store := storage.NewMemoryStore()
	require.NoError(t, store.Put(ctx, []byte("slow"), []byte("data")))
	require.NoError(t, store.Put(ctx, []byte("healthy"), []byte("data")))

	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()
	slow := &asyncCapturePeer{writeStarted: make(chan struct{}), writeRelease: releaseSlow}
	healthy := &asyncCapturePeer{}
	limits := replication.Limits{MaxDataBytes: 16}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	slowDone := make(chan error, 1)
	go func() {
		slowDone <- sendRequestedBlobs(ctx, slow, store, [][]byte{[]byte("slow")}, limits, 0, metrics, log, true)
	}()
	select {
	case <-slow.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("slow repair did not reach its write")
	}
	assert.Equal(t, int64(1), metrics.RepairIOOpsInFlight.Load())

	healthyDone := make(chan error, 1)
	go func() {
		healthyDone <- sendRequestedBlobs(ctx, healthy, store, [][]byte{[]byte("healthy")}, limits, 0, metrics, log, true)
	}()
	require.Eventually(t, func() bool {
		return metrics.RepairIOOpsQueued.Load() == 1
	}, time.Second, time.Millisecond)
	select {
	case <-healthyDone:
		t.Fatal("healthy repair bypassed the held global budget")
	default:
	}

	release()
	require.NoError(t, <-slowDone)
	require.NoError(t, <-healthyDone)
	t.Logf(
		"repair I/O saturation: max_ops=1 in_flight_before_release=1 queued_before_release=1 healthy_progress_after_release=true",
	)
	assert.Equal(t, 2, healthy.Len()+slow.Len())
	assert.Equal(t, uint64(2), metrics.RepairIOOpsStarted.Load())
	assert.Equal(t, uint64(2), metrics.RepairIOOpsCompleted.Load())
	assert.Equal(t, uint64(1), metrics.RepairIOOpsWaited.Load())
	assert.Zero(t, metrics.RepairIOOpsRejected.Load())
	assert.Zero(t, metrics.RepairIOOpsInFlight.Load())
	assert.Zero(t, metrics.RepairIOOpsQueued.Load())
}

func TestRepairIOLimiterRejectsCanceledWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics := &replicationMetrics{}
	limiter := newRepairIOLimiter(1, metrics)
	hold, err := limiter.acquire(ctx)
	require.NoError(t, err)

	waitCtx, cancelWait := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, acquireErr := limiter.acquire(waitCtx)
		done <- acquireErr
	}()
	require.Eventually(t, func() bool {
		return metrics.RepairIOOpsQueued.Load() == 1
	}, time.Second, time.Millisecond)
	cancelWait()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, uint64(1), metrics.RepairIOOpsRejected.Load())
	assert.Zero(t, metrics.RepairIOOpsQueued.Load())

	hold()
	assert.Equal(t, uint64(1), metrics.RepairIOOpsCompleted.Load())
	assert.Zero(t, metrics.RepairIOOpsInFlight.Load())
}

func BenchmarkRepairIOLimiterSaturation(b *testing.B) {
	for _, maxOps := range []int{1, defaultMaxRepairOps} {
		b.Run("max_ops_"+strconv.Itoa(maxOps), func(b *testing.B) {
			ctx := context.Background()
			metrics := &replicationMetrics{}
			limiter := newRepairIOLimiter(maxOps, metrics)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				holds := make([]func(), maxOps)
				for j := range holds {
					release, err := limiter.acquire(ctx)
					if err != nil {
						b.Fatal(err)
					}
					holds[j] = release
				}
				result := make(chan error, 1)
				go func() {
					release, err := limiter.acquire(ctx)
					if err == nil {
						release()
					}
					result <- err
				}()
				for metrics.RepairIOOpsQueued.Load() == 0 {
					runtime.Gosched()
				}
				holds[0]()
				if err := <-result; err != nil {
					b.Fatal(err)
				}
				for _, release := range holds[1:] {
					release()
				}
			}
		})
	}
}
