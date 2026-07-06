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
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func main() {
	var (
		workersCSV  = flag.String("workers", "http://localhost:8080", "comma-separated worker base URLs")
		states      = flag.Int("states", 2, "number of machine states to enumerate")
		maxSteps    = flag.Int64("max-steps", 1000, "per-candidate step limit; machines that don't halt within it are treated as non-halters")
		concurrency = flag.Int("concurrency", 16, "number of in-flight requests")
		force       = flag.Bool("force", false, "run even if the search space exceeds the safety threshold")
	)
	flag.Parse()

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

	// Guard against accidentally enumerating an astronomically large space.
	total := spaceSize(*states)
	const threshold = 1e7
	if total > threshold && !*force {
		fatal(fmt.Sprintf("search space for %d states is %.0f machines, above the %.0e safety threshold; pass -force to proceed anyway",
			*states, total, threshold))
	}

	slog.Info("starting search",
		"states", *states,
		"candidates", total,
		"workers", len(workers),
		"concurrency", *concurrency,
		"max_steps", *maxSteps,
	)

	start := time.Now()
	result, err := Search(context.Background(), cfg)
	if err != nil {
		fatal(err.Error())
	}
	elapsed := time.Since(start)

	printReport(os.Stdout, *states, result, elapsed)
}

// Config parameterises a search.
type Config struct {
	Workers     []string
	States      int
	MaxSteps    int64
	Concurrency int
	Client      *http.Client
}

// Champion records the best machine found for a given metric.
type Champion struct {
	Program *turing.Program
	Steps   int64
	Ones    int
}

// SearchResult summarises a completed search.
type SearchResult struct {
	Total   int // candidates enumerated
	Halted  int // candidates that halted within the step limit
	Errored int // candidates whose worker call failed at the machine level

	StepChampion Champion // most steps before halting
	OnesChampion Champion // most 1s left on the tape
}

// Search enumerates every candidate machine and fans it out to the workers,
// returning the champions once every candidate has been evaluated.
func Search(ctx context.Context, cfg Config) (SearchResult, error) {
	alphabet, stateNames := buildAlphabet(cfg.States)

	programs := make(chan *turing.Program)
	go enumerate(stateNames, alphabet, programs)

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
					o.ones = countOnes(resp.Snapshot.Tape)
				}
				outcomes <- o
			}
		}()
	}
	go func() { wg.Wait(); close(outcomes) }()

	var result SearchResult
	var processed int
	for o := range outcomes {
		if o.terr != nil {
			return result, fmt.Errorf("worker call failed: %w", o.terr)
		}
		result.Total++
		processed++
		if o.merr != "" && !o.halted {
			// Expected for non-halters that hit the step limit.
		}
		if o.halted {
			result.Halted++
			if o.steps > result.StepChampion.Steps {
				result.StepChampion = Champion{Program: o.prog, Steps: o.steps, Ones: o.ones}
			}
			if o.ones > result.OnesChampion.Ones {
				result.OnesChampion = Champion{Program: o.prog, Steps: o.steps, Ones: o.ones}
			}
		}
		if processed%2000 == 0 {
			slog.Info("progress", "evaluated", processed, "halted", result.Halted)
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

// transition is one entry in the per-cell alphabet of possible moves.
type transition struct {
	write byte
	move  turing.Direction
	next  string
}

// buildAlphabet returns the set of possible transitions for a cell and the
// state names A, B, C, ... for an n-state machine. A cell may write 0 or 1,
// move L or R, and transition to any state or HALT.
func buildAlphabet(states int) ([]transition, []string) {
	names := make([]string, states)
	for i := range names {
		names[i] = string(rune('A' + i))
	}
	nexts := append(append([]string{}, names...), turing.Halt)

	var alphabet []transition
	for _, w := range []byte{0, 1} {
		for _, mv := range []turing.Direction{turing.Left, turing.Right} {
			for _, nx := range nexts {
				alphabet = append(alphabet, transition{write: w, move: mv, next: nx})
			}
		}
	}
	return alphabet, names
}

// enumerate streams every complete transition table over the given states and
// alphabet as a Program. There are len(alphabet)^(2*states) of them. It closes
// out when done.
func enumerate(states []string, alphabet []transition, out chan<- *turing.Program) {
	defer close(out)
	cells := 2 * len(states) // (state, symbol) pairs
	k := len(alphabet)
	assign := make([]int, cells)
	for {
		out <- buildProgram(states, alphabet, assign)

		// Increment the mixed-radix odometer over cells.
		i := 0
		for ; i < cells; i++ {
			assign[i]++
			if assign[i] < k {
				break
			}
			assign[i] = 0
		}
		if i == cells {
			return // rolled over — enumerated everything
		}
	}
}

// buildProgram materialises the Program described by one cell->transition
// assignment. Cell c encodes state c/2 reading symbol c%2.
func buildProgram(states []string, alphabet []transition, assign []int) *turing.Program {
	rules := make([]turing.Rule, len(assign))
	for cell, ti := range assign {
		t := alphabet[ti]
		rules[cell] = turing.Rule{
			State: states[cell/2],
			Read:  byte(cell % 2),
			Write: t.write,
			Move:  t.move,
			Next:  t.next,
		}
	}
	return &turing.Program{
		Name:       "candidate",
		StartState: states[0],
		Rules:      rules,
	}
}

// countOnes counts the non-blank cells on a snapshot tape. In a 2-symbol
// machine every non-blank cell holds a 1.
func countOnes(tape map[int]byte) int {
	n := 0
	for _, v := range tape {
		if v != turing.Blank {
			n++
		}
	}
	return n
}

// spaceSize returns the number of candidate machines for an n-state machine.
func spaceSize(states int) float64 {
	k := float64(2 * 2 * (states + 1)) // write * move * next
	return math.Pow(k, float64(2*states))
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
