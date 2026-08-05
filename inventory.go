package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/AliSinaDevelo/StreamHive/replication"
	"github.com/AliSinaDevelo/StreamHive/storage"
)

const (
	defaultMaxInventoryBytes = 16 << 20
	defaultMaxInventoryKeys  = replication.DefaultMaxKeys * 4
)

type inventoryExchangeBudget struct {
	maxBytes int
	maxKeys  int
}

type inventoryExchangeScheduler struct {
	ctx           context.Context
	lister        storage.BlobKeyLister
	limits        replication.Limits
	maxFrameBytes int
	budget        inventoryExchangeBudget
	metrics       *replicationMetrics
	log           *slog.Logger
	delay         time.Duration

	mu      sync.Mutex
	entries map[string]*inventoryExchangeEntry
}

type inventoryExchangeEntry struct {
	peer      p2p.Peer
	cursor    []byte
	running   bool
	scheduled bool
}

func newInventoryExchangeScheduler(
	ctx context.Context,
	lister storage.BlobKeyLister,
	limits replication.Limits,
	maxFrameBytes int,
	budget inventoryExchangeBudget,
	metrics *replicationMetrics,
	log *slog.Logger,
	delay time.Duration,
) *inventoryExchangeScheduler {
	if delay <= 0 {
		delay = repairContinuationDelay
	}
	return &inventoryExchangeScheduler{
		ctx:           ctx,
		lister:        lister,
		limits:        limits,
		maxFrameBytes: maxFrameBytes,
		budget:        budget,
		metrics:       metrics,
		log:           log,
		delay:         delay,
		entries:       make(map[string]*inventoryExchangeEntry),
	}
}

func (s *inventoryExchangeScheduler) Start(peer p2p.Peer) {
	if s == nil || peer == nil || s.ctx.Err() != nil {
		return
	}
	id := repairPeerKey(peer)
	launch := false
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		entry = &inventoryExchangeEntry{peer: peer}
		s.entries[id] = entry
		if s.metrics != nil {
			s.metrics.InventoryExchangesActive.Add(1)
		}
	}
	entry.peer = peer
	if !entry.running && !entry.scheduled {
		entry.scheduled = true
		launch = true
	}
	s.mu.Unlock()

	if launch {
		go s.run(id)
	}
}

func (s *inventoryExchangeScheduler) Forget(peer p2p.Peer) {
	if s == nil || peer == nil {
		return
	}
	s.forgetID(repairPeerKey(peer))
}

func (s *inventoryExchangeScheduler) run(id string) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry == nil {
		s.mu.Unlock()
		return
	}
	entry.scheduled = false
	entry.running = true
	peer := entry.peer
	cursor := append([]byte(nil), entry.cursor...)
	s.mu.Unlock()

	if s.metrics != nil {
		s.metrics.InventoryExchangesStarted.Add(1)
	}
	next, complete, err := sendInventoryExchange(
		s.ctx,
		peer,
		s.lister,
		s.limits,
		s.maxFrameBytes,
		s.budget,
		cursor,
		s.metrics,
	)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if s.metrics != nil {
				s.metrics.SendErrors.Add(1)
				s.metrics.InventoryExchangesDropped.Add(1)
			}
			s.log.Error("replication inventory exchange failed", "remote", peer.RemoteAddr().String(), "err", err)
			_ = peer.Close()
		} else if s.metrics != nil {
			s.metrics.InventoryExchangesDropped.Add(1)
		}
		s.forgetID(id)
		return
	}

	if complete {
		if s.metrics != nil {
			s.metrics.InventoryExchangesCompleted.Add(1)
		}
		s.finish(id)
		return
	}
	if s.metrics != nil {
		s.metrics.InventoryExchangesLimited.Add(1)
	}

	s.mu.Lock()
	entry = s.entries[id]
	if entry == nil {
		s.mu.Unlock()
		return
	}
	entry.running = false
	entry.cursor = append(entry.cursor[:0], next...)
	if !entry.scheduled {
		entry.scheduled = true
	}
	s.mu.Unlock()

	go s.wait(id)
}

func (s *inventoryExchangeScheduler) wait(id string) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		s.forgetID(id)
	case <-timer.C:
		s.run(id)
	}
}

func (s *inventoryExchangeScheduler) finish(id string) {
	s.mu.Lock()
	_, ok := s.entries[id]
	if ok {
		delete(s.entries, id)
	}
	s.mu.Unlock()
	if ok && s.metrics != nil {
		s.metrics.InventoryExchangesActive.Add(-1)
	}
}

func (s *inventoryExchangeScheduler) forgetID(id string) {
	s.mu.Lock()
	_, ok := s.entries[id]
	if ok {
		delete(s.entries, id)
	}
	s.mu.Unlock()
	if ok && s.metrics != nil {
		s.metrics.InventoryExchangesActive.Add(-1)
	}
}

func sendInventoryExchange(
	ctx context.Context,
	peer p2p.Peer,
	lister storage.BlobKeyLister,
	limits replication.Limits,
	maxFrameBytes int,
	budget inventoryExchangeBudget,
	after []byte,
	metrics *replicationMetrics,
) ([]byte, bool, error) {
	if pager, ok := lister.(storage.BlobKeyPager); ok {
		return sendPagedBlobHasBudget(ctx, peer, pager, limits, maxFrameBytes, budget, after, metrics)
	}
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i], keys[j]) < 0
	})
	start := sort.Search(len(keys), func(i int) bool {
		return len(after) == 0 || bytes.Compare(keys[i], after) > 0
	})
	if start == len(keys) {
		return append([]byte(nil), after...), true, nil
	}
	last, complete, _, _, err := sendBlobHasKeysBudget(ctx, peer, keys[start:], limits, maxFrameBytes, budget, 0, 0, metrics)
	return last, complete, err
}

func sendPagedBlobHasBudget(
	ctx context.Context,
	peer p2p.Peer,
	pager storage.BlobKeyPager,
	limits replication.Limits,
	maxFrameBytes int,
	budget inventoryExchangeBudget,
	after []byte,
	metrics *replicationMetrics,
) ([]byte, bool, error) {
	pageSize := inventoryBatchSize(limits)
	if budget.maxKeys > 0 && budget.maxKeys < pageSize {
		pageSize = budget.maxKeys
	}
	if pageSize <= 0 {
		pageSize = 1
	}
	cursor := append([]byte(nil), after...)
	sentKeys := 0
	sentBytes := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if budget.maxKeys > 0 && sentKeys >= budget.maxKeys {
			return cursor, false, nil
		}
		limit := pageSize
		if budget.maxKeys > 0 && limit > budget.maxKeys-sentKeys {
			limit = budget.maxKeys - sentKeys
		}
		keys, next, err := pager.ListKeyPage(ctx, cursor, limit)
		if err != nil {
			return nil, false, err
		}
		if len(keys) == 0 {
			return cursor, true, nil
		}
		if len(next) == 0 || !bytes.Equal(next, keys[len(keys)-1]) {
			return nil, false, errors.New("replication: blob key page cursor must be last key")
		}
		if len(cursor) > 0 && bytes.Compare(next, cursor) <= 0 {
			return nil, false, errors.New("replication: blob key page cursor did not advance")
		}
		last, complete, totalKeys, totalBytes, err := sendBlobHasKeysBudget(
			ctx,
			peer,
			keys,
			limits,
			maxFrameBytes,
			budget,
			sentKeys,
			sentBytes,
			metrics,
		)
		if err != nil {
			return nil, false, err
		}
		sentKeys = totalKeys
		sentBytes = totalBytes
		if !complete {
			return last, false, nil
		}
		cursor = append(cursor[:0], next...)
	}
}

func sendBlobHasKeysBudget(
	ctx context.Context,
	peer p2p.Peer,
	keys [][]byte,
	limits replication.Limits,
	maxFrameBytes int,
	budget inventoryExchangeBudget,
	sentKeys int,
	sentBytes int,
	metrics *replicationMetrics,
) ([]byte, bool, int, int, error) {
	if len(keys) == 0 {
		return nil, true, sentKeys, sentBytes, nil
	}
	batchSize := inventoryBatchSize(limits)
	maxPayload := maxFrameBytes
	if maxPayload <= 0 {
		maxPayload = p2p.DefaultMaxFrameBytes
	}
	var cursor []byte
	for start := 0; start < len(keys); {
		if budget.maxKeys > 0 && sentKeys >= budget.maxKeys {
			return cursor, false, sentKeys, sentBytes, nil
		}
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		if budget.maxKeys > 0 && end-start > budget.maxKeys-sentKeys {
			end = start + budget.maxKeys - sentKeys
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, false, sentKeys, sentBytes, err
			}
			payload, err := replication.EncodeBlobHas(keys[start:end], limits)
			if err != nil {
				return nil, false, sentKeys, sentBytes, err
			}
			if len(payload) > maxPayload {
				if end-start == 1 {
					return nil, false, sentKeys, sentBytes, p2p.ErrFrameTooLarge
				}
				end = start + (end-start)/2
				continue
			}
			if budget.maxBytes > 0 && sentBytes+len(payload) > budget.maxBytes {
				if end-start > 1 {
					end = start + (end-start)/2
					continue
				}
				if sentKeys > 0 {
					return cursor, false, sentKeys, sentBytes, nil
				}
				// A single frame is the minimum progress unit. This keeps a
				// deliberately tiny budget from trapping one valid key forever.
			}
			if err := writePeerFrame(peer, payload, maxFrameBytes); err != nil {
				return nil, false, sentKeys, sentBytes, err
			}
			if metrics != nil {
				metrics.InventoryAdvertisements.Add(1)
				metrics.InventoryBytesSent.Add(uint64(len(payload)))
				metrics.InventoryKeysSent.Add(uint64(end - start))
			}
			sentKeys += end - start
			sentBytes += len(payload)
			cursor = append(cursor[:0], keys[end-1]...)
			start = end
			break
		}
	}
	return cursor, true, sentKeys, sentBytes, nil
}
