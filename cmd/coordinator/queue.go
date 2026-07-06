package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/queue"
	"github.com/russellwallace/turing-cluster/pkg/search"
)

// RunQueueSearch drives a search over the Redis work queue instead of direct
// HTTP fan-out. It enqueues one batch per index range onto the jobs stream, then
// folds the per-batch outcomes the workers publish back onto the results stream
// until every batch has reported. The workers do the actual evaluation; the
// coordinator only produces work and aggregates champions.
func RunQueueSearch(ctx context.Context, redisAddr string, states int, batchSize uint64, stepLimit int64) (SearchResult, error) {
	qc := queue.Dial(redisAddr)
	defer qc.Close()

	if err := qc.Ping(ctx); err != nil {
		return SearchResult{}, fmt.Errorf("connect to redis %s: %w", redisAddr, err)
	}
	if err := qc.EnsureGroup(ctx); err != nil {
		return SearchResult{}, fmt.Errorf("ensure consumer group: %w", err)
	}

	// Read results strictly after whatever is already on the stream, so outcomes
	// left by a previous run are not counted toward this one.
	cursor, err := qc.LastResultID(ctx)
	if err != nil {
		return SearchResult{}, fmt.Errorf("read results cursor: %w", err)
	}

	batches := search.Enumerate(states, batchSize)
	for _, b := range batches {
		if err := qc.EnqueueBatch(ctx, b); err != nil {
			return SearchResult{}, fmt.Errorf("enqueue batch @%d: %w", b.Start, err)
		}
	}
	slog.Info("enqueued batches", "batches", len(batches), "batch_size", batchSize)

	var result SearchResult
	completed := 0
	for completed < len(batches) {
		outcomes, next, err := qc.ReadOutcomes(ctx, cursor, 64, 5*time.Second)
		if err != nil {
			return result, fmt.Errorf("read outcomes: %w", err)
		}
		cursor = next
		for _, o := range outcomes {
			result.Merge(o.Tally)
			completed++
		}
		if len(outcomes) > 0 {
			slog.Info("progress", "completed_batches", completed, "total_batches", len(batches), "halted", result.Halted)
		}
	}

	if err := qc.StoreChampions(ctx, result); err != nil {
		slog.Warn("mirror champions to redis failed", "err", err)
	}
	return result, nil
}
