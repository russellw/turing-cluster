package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

// fakeWorker stands up an httptest server that implements the same POST /run
// contract as cmd/server, running each machine via the turing package. This
// lets the coordinator's enumeration, fan-out and aggregation be exercised
// end-to-end without external infrastructure.
func fakeWorker(t *testing.T) *httptest.Server {
	t.Helper()
	h := http.NewServeMux()
	h.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req runReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m, err := turing.New(req.Program, req.MaxSteps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		runErr := m.Run()
		resp := runResp{Snapshot: m.Snapshot(), Halted: m.Halted()}
		if runErr != nil {
			resp.Error = runErr.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(h)
}

func TestSearchN2FindsBusyBeaver2(t *testing.T) {
	srv := fakeWorker(t)
	defer srv.Close()

	cfg := Config{
		Workers:     []string{srv.URL},
		States:      2,
		MaxSteps:    1000,
		Concurrency: 8,
		Client:      &http.Client{Timeout: 30 * time.Second},
	}

	result, err := Search(context.Background(), cfg)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	// Full 2-state 2-symbol space: 12^4 = 20,736 machines.
	if want := 20736; result.Total != want {
		t.Errorf("Total = %d, want %d", result.Total, want)
	}
	// The known busy-beaver values for n=2.
	if result.StepChampion.Steps != 6 {
		t.Errorf("S(2) = %d, want 6", result.StepChampion.Steps)
	}
	if result.OnesChampion.Ones != 4 {
		t.Errorf("sigma(2) = %d, want 4", result.OnesChampion.Ones)
	}
	if result.Halted == 0 {
		t.Error("no halting machines found")
	}
}

func TestSpaceSize(t *testing.T) {
	if got := spaceSize(2); got != 20736 {
		t.Errorf("spaceSize(2) = %v, want 20736", got)
	}
}

func TestEnumerateProducesCompleteDistinctTables(t *testing.T) {
	alphabet, states := buildAlphabet(2)
	ch := make(chan *turing.Program)
	go enumerate(states, alphabet, ch)

	count := 0
	seen := make(map[string]bool)
	for p := range ch {
		count++
		// Every table must define all 2*states cells and be loadable
		// (no duplicate/missing rules).
		if len(p.Rules) != 4 {
			t.Fatalf("program has %d rules, want 4", len(p.Rules))
		}
		if _, err := turing.New(p, 0); err != nil {
			t.Fatalf("enumerated invalid program: %v", err)
		}
		key, _ := json.Marshal(p.Rules)
		if seen[string(key)] {
			t.Fatalf("duplicate program enumerated: %s", key)
		}
		seen[string(key)] = true
	}
	if count != 20736 {
		t.Errorf("enumerated %d programs, want 20736", count)
	}
}
