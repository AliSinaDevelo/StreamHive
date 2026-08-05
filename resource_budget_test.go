package main

import (
	"context"
	"io"
	"log/slog"
	"strconv"
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
