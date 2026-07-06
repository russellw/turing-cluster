package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/queue"
	"github.com/russellwallace/turing-cluster/pkg/search"
	"github.com/russellwallace/turing-cluster/pkg/turing"
)

// runConsumer drives the queue-consumer loop: reclaim any abandoned work, read
// new batches assigned to this consumer, evaluate each, and publish its outcome.
// It returns when ctx is cancelled (SIGTERM), after finishing whatever batch is
// in flight — the per-batch commit uses its own background context so a batch is
// never abandoned mid-drain.
func runConsumer(ctx context.Context, qc *queue.Client, stepLimit int64) error {
	consumer, _ := os.Hostname()
	if consumer == "" {
		consumer = "worker"
	}
	if err := qc.EnsureGroup(ctx); err != nil {
		return fmt.Errorf("ensure consumer group: %w", err)
	}
	slog.Info("queue consumer started", "consumer", consumer, "step_limit", stepLimit)

	const readCount = 4
	for {
		if ctx.Err() != nil {
			slog.Info("queue consumer stopped", "consumer", consumer)
			return nil
		}

		// Reclaim batches a crashed worker left pending (idle > 60s), then read
		// newly assigned batches.
		if claimed, err := qc.ClaimStale(ctx, consumer, 60*time.Second, readCount); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("claim stale jobs failed", "err", err)
		} else {
			for _, job := range claimed {
				slog.Info("reclaimed stale batch", "start", job.Batch.Start, "count", job.Batch.Count)
				processJob(qc, job, stepLimit)
			}
		}

		jobs, err := qc.ReadJobs(ctx, consumer, readCount, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("read jobs failed", "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, job := range jobs {
			processJob(qc, job, stepLimit)
		}
	}
}

// processJob evaluates every machine in a batch, publishes the folded outcome,
// and acks the job. It uses a fresh bounded context (not the loop's cancellable
// one) so a batch already picked up completes and acks even during shutdown; a
// job whose outcome fails to publish is left un-acked and will be reclaimed.
func processJob(qc *queue.Client, job queue.Job, stepLimit int64) {
	var tally search.Tally
	search.Expand(job.Batch, func(_ uint64, p *turing.Program) bool {
		m, err := turing.New(p, stepLimit)
		if err != nil {
			// Enumerated machines are always well-formed; count defensively.
			tally.Add(p, false, 0, 0)
			return true
		}
		_ = m.Run() // step-limit error is the expected non-halter signal
		snap := m.Snapshot()
		tally.Add(p, m.Halted(), snap.Steps, search.CountOnes(snap.Tape))
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := qc.PublishOutcome(ctx, queue.Outcome{Batch: job.Batch, Tally: tally}); err != nil {
		slog.Error("publish outcome failed; leaving job for reclaim", "start", job.Batch.Start, "err", err)
		return
	}
	if err := qc.Ack(ctx, job.ID); err != nil {
		slog.Error("ack failed", "id", job.ID, "err", err)
	}
}
