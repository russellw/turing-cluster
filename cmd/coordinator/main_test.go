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
