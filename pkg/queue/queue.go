// Package queue is the Redis-backed work queue that decouples the coordinator
// (producer) from the workers (consumers). The coordinator enqueues Batches of
// machine indices onto the "jobs" stream; workers consume them via the
// "workers" consumer group, expand and run each batch locally, and publish a
// per-batch Outcome onto the "results" stream. The coordinator folds those
// outcomes into the global champions and mirrors them to the "champions" hash.
//
// Delivery is at-least-once: a worker acks a job only after its outcome is
// durably on the results stream, and jobs left pending by a crashed worker are
// reclaimed via XAUTOCLAIM. Reprocessing is safe because Outcome folding is by
// max (see search.Tally) — the only cost of a redelivery is recomputation.
//
// See docs/DESIGN-queue-observability.md.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/russellwallace/turing-cluster/pkg/search"
)

// Stream, group, and hash names.
const (
	StreamJobs    = "jobs"      // batches to evaluate
	StreamResults = "results"   // per-batch outcomes
	GroupWorkers  = "workers"   // consumer group over StreamJobs
	HashChampions = "champions" // mirrored high-water-mark state
)

// Client wraps a go-redis client with the search's domain operations.
type Client struct {
	rdb *redis.Client
}

// Dial constructs a Client for the given Redis address (host:port).
func Dial(addr string) *Client {
	return &Client{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// Ping checks connectivity.
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// Close releases the underlying connection pool.
func (c *Client) Close() error { return c.rdb.Close() }

// EnsureGroup creates the workers consumer group (and the jobs stream if
// absent), starting at "0" so the group consumes from the beginning of the
// stream regardless of whether the group or the jobs were created first. It is
// idempotent: an existing group is not an error.
func (c *Client) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, StreamJobs, GroupWorkers, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// EnqueueBatch appends one batch to the jobs stream.
func (c *Client) EnqueueBatch(ctx context.Context, b search.Batch) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamJobs,
		Values: map[string]any{"batch": data},
	}).Err()
}

// Job is a batch delivered to a consumer, tagged with the stream message ID
// needed to ack it.
type Job struct {
	ID    string
	Batch search.Batch
}

// ReadJobs blocks up to block for new jobs assigned to this consumer, returning
// at most count of them. A nil slice with nil error means the block timed out
// with nothing available.
func (c *Client) ReadJobs(ctx context.Context, consumer string, count int, block time.Duration) ([]Job, error) {
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    GroupWorkers,
		Consumer: consumer,
		Streams:  []string{StreamJobs, ">"},
		Count:    int64(count),
		Block:    block,
	}).Result()
	if err == redis.Nil {
		return nil, nil // timed out, no messages
	}
	if err != nil {
		return nil, err
	}
	return jobsFromStreams(res)
}

// ClaimStale reclaims jobs that were delivered to some consumer but left
// unacked for longer than minIdle — i.e. a worker that crashed mid-batch. The
// reclaimed jobs are reassigned to this consumer.
func (c *Client) ClaimStale(ctx context.Context, consumer string, minIdle time.Duration, count int) ([]Job, error) {
	msgs, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   StreamJobs,
		Group:    GroupWorkers,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    "0",
		Count:    int64(count),
	}).Result()
	if err != nil {
		return nil, err
	}
	return jobsFromMessages(msgs)
}

// Ack marks jobs as successfully processed, removing them from the group's
// pending list.
func (c *Client) Ack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return c.rdb.XAck(ctx, StreamJobs, GroupWorkers, ids...).Err()
}

// Outcome is a worker's folded result for one batch: the batch it covers and a
// Tally holding that batch's counts and local champions.
type Outcome struct {
	Batch search.Batch `json:"batch"`
	Tally search.Tally `json:"tally"`
}

// PublishOutcome appends a batch outcome to the results stream.
func (c *Client) PublishOutcome(ctx context.Context, o Outcome) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamResults,
		Values: map[string]any{"outcome": data},
	}).Err()
}

// LastResultID returns the ID of the most recent message on the results stream,
// or "0" if the stream is empty. The coordinator captures this before enqueuing
// so it reads only the outcomes for its own run, not any left by a prior one.
func (c *Client) LastResultID(ctx context.Context) (string, error) {
	msgs, err := c.rdb.XRevRangeN(ctx, StreamResults, "+", "-", 1).Result()
	if err != nil {
		return "0", err
	}
	if len(msgs) == 0 {
		return "0", nil
	}
	return msgs[0].ID, nil
}

// ReadOutcomes blocks up to block for outcomes published after lastID, returning
// them and the new cursor to pass on the next call.
func (c *Client) ReadOutcomes(ctx context.Context, lastID string, count int, block time.Duration) ([]Outcome, string, error) {
	res, err := c.rdb.XRead(ctx, &redis.XReadArgs{
		Streams: []string{StreamResults, lastID},
		Count:   int64(count),
		Block:   block,
	}).Result()
	if err == redis.Nil {
		return nil, lastID, nil // timed out, no new outcomes
	}
	if err != nil {
		return nil, lastID, err
	}
	var out []Outcome
	newID := lastID
	for _, s := range res {
		for _, m := range s.Messages {
			raw, ok := m.Values["outcome"].(string)
			if !ok {
				return nil, lastID, fmt.Errorf("results message %s missing outcome field", m.ID)
			}
			var o Outcome
			if err := json.Unmarshal([]byte(raw), &o); err != nil {
				return nil, lastID, err
			}
			out = append(out, o)
			newID = m.ID
		}
	}
	return out, newID, nil
}

// StoreChampions mirrors the global champion state to the champions hash so it
// survives a coordinator restart and can be inspected out of band.
func (c *Client) StoreChampions(ctx context.Context, t search.Tally) error {
	stepProg, err := json.Marshal(t.StepChampion.Program)
	if err != nil {
		return err
	}
	onesProg, err := json.Marshal(t.OnesChampion.Program)
	if err != nil {
		return err
	}
	return c.rdb.HSet(ctx, HashChampions, map[string]any{
		"total":        t.Total,
		"halted":       t.Halted,
		"steps":        t.StepChampion.Steps,
		"sigma":        t.OnesChampion.Ones,
		"step_program": stepProg,
		"ones_program": onesProg,
	}).Err()
}

func jobsFromStreams(streams []redis.XStream) ([]Job, error) {
	var jobs []Job
	for _, s := range streams {
		js, err := jobsFromMessages(s.Messages)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, js...)
	}
	return jobs, nil
}

func jobsFromMessages(msgs []redis.XMessage) ([]Job, error) {
	jobs := make([]Job, 0, len(msgs))
	for _, m := range msgs {
		raw, ok := m.Values["batch"].(string)
		if !ok {
			return nil, fmt.Errorf("jobs message %s missing batch field", m.ID)
		}
		var b search.Batch
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, err
		}
		jobs = append(jobs, Job{ID: m.ID, Batch: b})
	}
	return jobs, nil
}
