// Command coordinator runs a brute-force Busy Beaver search by enumerating
// every n-state, 2-symbol Turing machine and fanning the candidates out to a
// fleet of worker pods over POST /run. It collects the returned snapshots and
// reports the champions: the machine that ran the most steps before halting
// (S(n)) and the one that left the most 1s on the tape (sigma(n)).
//
// This is the first-cut coordinator. It enumerates the *full* transition space
// with no symmetry reduction, so it is only practical for very small n. For
// n=2 that space is 12^4 = 20,736 machines, which is fine. Reducing the space
// with normal-form / symmetry breaking is a follow-up (see REPORT.md).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/search"
	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func main() {
	var (
		workersCSV  = flag.String("workers", "http://localhost:8080", "comma-separated worker base URLs (HTTP fan-out mode)")
		redisAddr   = flag.String("redis", os.Getenv("REDIS_ADDR"), "redis host:port; if set, distribute work via the queue instead of HTTP fan-out")
		batchSize   = flag.Int64("batch", envInt64("BATCH_SIZE", 500), "machines per queued batch (queue mode)")
		metricsAddr = flag.String("metrics-addr", envOr("METRICS_ADDR", ":2112"), "address to serve Prometheus /metrics on in queue mode; empty to disable")
		pushgateway = flag.String("pushgateway", os.Getenv("PUSHGATEWAY_ADDR"), "Prometheus Pushgateway host:port; if set, push the final run summary there")
		states      = flag.Int("states", 2, "number of machine states to enumerate")
		maxSteps    = flag.Int64("max-steps", 1000, "per-candidate step limit; machines that don't halt within it are treated as non-halters")
		concurrency = flag.Int("concurrency", 16, "number of in-flight requests (HTTP fan-out mode)")
		force       = flag.Bool("force", false, "run even if the search space exceeds the safety threshold")
	)
	flag.Parse()

	// Guard against accidentally enumerating an astronomically large space.
	total := search.SpaceSizeFloat(*states)
	const threshold = 1e7
	if total > threshold && !*force {
		fatal(fmt.Sprintf("search space for %d states is %.0f machines, above the %.0e safety threshold; pass -force to proceed anyway",
			*states, total, threshold))
	}

	var (
		result SearchResult
		err    error
		start  = time.Now()
	)
	if *redisAddr != "" {
		if *metricsAddr != "" {
			serveMetrics(*metricsAddr)
		}
		slog.Info("starting queue search",
			"states", *states, "candidates", total,
			"redis", *redisAddr, "batch", *batchSize, "max_steps", *maxSteps,
		)
		result, err = RunQueueSearch(context.Background(), *redisAddr, *states, uint64(*batchSize), *maxSteps)
	} else {
		workers := splitWorkers(*workersCSV)
		if len(workers) == 0 {
			fatal("no workers configured")
		}
		cfg := Config{
			Workers:     workers,
			States:      *states,
			MaxSteps:    *maxSteps,
			Concurrency: *concurrency,
			Client:      &http.Client{Timeout: 30 * time.Second},
		}
		slog.Info("starting search",
			"states", *states, "candidates", total,
			"workers", len(workers), "concurrency", *concurrency, "max_steps", *maxSteps,
		)
		result, err = Search(context.Background(), cfg)
	}
	if err != nil {
		fatal(err.Error())
	}

	// In queue mode, push the final summary to the Pushgateway so a run-once
	// Job's champions and totals outlive the process and reach the dashboard.
	if *redisAddr != "" && *pushgateway != "" {
		pushMetrics(*pushgateway)
	}

	printReport(os.Stdout, *states, result, time.Since(start))
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Config parameterises a search.
type Config struct {
	Workers     []string
	States      int
	MaxSteps    int64
	Concurrency int
	Client      *http.Client
}

// SearchResult summarises a completed search. It is the search package's champion
// accumulator: Total, Halted, StepChampion (S(n)) and OnesChampion (sigma(n)).
type SearchResult = search.Tally

// Search enumerates every candidate machine and fans it out to the workers,
// returning the champions once every candidate has been evaluated.
func Search(ctx context.Context, cfg Config) (SearchResult, error) {
	programs := make(chan *turing.Program)
	go func() {
		defer close(programs)
		search.Programs(cfg.States, 0, search.SpaceSize(cfg.States), func(_ uint64, p *turing.Program) bool {
			programs <- p
			return true
		})
	}()

	type outcome struct {
		prog   *turing.Program
		halted bool
		steps  int64
		ones   int
		merr   string // machine-level error (e.g. step limit reached)
		terr   error  // transport-level error — aborts the search
	}
	outcomes := make(chan outcome, cfg.Concurrency)

	var rr uint64 // round-robin worker selector
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for prog := range programs {
				worker := cfg.Workers[int(atomic.AddUint64(&rr, 1))%len(cfg.Workers)]
				resp, err := runCandidate(ctx, cfg.Client, worker, prog, cfg.MaxSteps)
				if err != nil {
					outcomes <- outcome{prog: prog, terr: err}
					continue
				}
				o := outcome{prog: prog, halted: resp.Halted, merr: resp.Error}
				if resp.Snapshot != nil {
					o.steps = resp.Snapshot.Steps
					o.ones = search.CountOnes(resp.Snapshot.Tape)
				}
				outcomes <- o
			}
		}()
	}
	go func() { wg.Wait(); close(outcomes) }()

	var result SearchResult
	for o := range outcomes {
		if o.terr != nil {
			return result, fmt.Errorf("worker call failed: %w", o.terr)
		}
		// A machine-level error with halted=false is expected: it's a non-halter
		// that hit the step limit. Add folds it in as a non-halter.
		result.Add(o.prog, o.halted, o.steps, o.ones)
		if result.Total%2000 == 0 {
			slog.Info("progress", "evaluated", result.Total, "halted", result.Halted)
		}
	}
	return result, nil
}

// runReq / runResp mirror the worker's POST /run contract (cmd/server).
type runReq struct {
	Program  *turing.Program `json:"program"`
	MaxSteps int64           `json:"max_steps"`
}

type runResp struct {
	Snapshot *turing.Snapshot `json:"snapshot"`
	Halted   bool             `json:"halted"`
	Error    string           `json:"error,omitempty"`
}

// runCandidate posts one program to a worker and returns its response. The
// returned error is non-nil only for transport/HTTP-level failures; a machine
// that fails to halt reports that in runResp.Error, not here.
func runCandidate(ctx context.Context, client *http.Client, worker string, prog *turing.Program, maxSteps int64) (runResp, error) {
	body, err := json.Marshal(runReq{Program: prog, MaxSteps: maxSteps})
	if err != nil {
		return runResp{}, err
	}
	url := strings.TrimRight(worker, "/") + "/run"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return runResp{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return runResp{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return runResp{}, fmt.Errorf("worker %s returned status %d", worker, resp.StatusCode)
	}
	var out runResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return runResp{}, err
	}
	return out, nil
}

func splitWorkers(csv string) []string {
	var out []string
	for _, w := range strings.Split(csv, ",") {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func printReport(w *os.File, states int, r SearchResult, elapsed time.Duration) {
	fmt.Fprintf(w, "\n=== Busy Beaver search: %d states, 2 symbols ===\n", states)
	fmt.Fprintf(w, "candidates evaluated : %d\n", r.Total)
	fmt.Fprintf(w, "halting machines     : %d\n", r.Halted)
	fmt.Fprintf(w, "elapsed              : %s\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "\nS(%d)  most steps before halting : %d\n", states, r.StepChampion.Steps)
	fmt.Fprintf(w, "sigma(%d)  most 1s on the tape     : %d\n", states, r.OnesChampion.Ones)
	if r.StepChampion.Program != nil {
		fmt.Fprintf(w, "\nstep champion program:\n%s\n", programJSON(r.StepChampion.Program))
	}
}

func programJSON(p *turing.Program) string {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}
