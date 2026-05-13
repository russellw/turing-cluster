package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("healthz: got %d, want 200", w.Code)
	}
}

func postRun(t *testing.T, body RunRequest) RunResponse {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(data))
	w := httptest.NewRecorder()
	handleRun(w, req)
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestRunBusyBeaver2(t *testing.T) {
	resp := postRun(t, RunRequest{Program: turing.BusyBeaver2})
	if !resp.Halted {
		t.Errorf("expected halted")
	}
	if resp.Error != "" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
	if resp.Snapshot.Steps != 6 {
		t.Errorf("steps = %d, want 6", resp.Snapshot.Steps)
	}
}

func TestRunWithStepLimit(t *testing.T) {
	resp := postRun(t, RunRequest{Program: turing.BusyBeaver4, MaxSteps: 50})
	if resp.Halted {
		t.Error("should not have halted within step limit")
	}
	if resp.Error == "" {
		t.Error("expected step-limit error")
	}
	if resp.Snapshot.Steps != 50 {
		t.Errorf("steps = %d, want 50", resp.Snapshot.Steps)
	}
}

func TestRunInvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	handleRun(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

func TestRunMissingProgram(t *testing.T) {
	resp := postRun(t, RunRequest{})
	if resp.Error == "" {
		t.Error("expected error for missing program")
	}
}

func TestRunDuplicateRule(t *testing.T) {
	bad := &turing.Program{
		Name:       "bad",
		StartState: "A",
		Rules: []turing.Rule{
			{State: "A", Read: 0, Write: 1, Move: turing.Right, Next: turing.Halt},
			{State: "A", Read: 0, Write: 0, Move: turing.Left, Next: turing.Halt},
		},
	}
	resp := postRun(t, RunRequest{Program: bad})
	if resp.Error == "" {
		t.Error("expected error for duplicate rule")
	}
}

func TestSnapshotRoundtrip(t *testing.T) {
	// Run BB4 to a snapshot, restore, run to completion — same result.
	resp1 := postRun(t, RunRequest{Program: turing.BusyBeaver4, MaxSteps: 50})

	m, err := turing.Restore(resp1.Snapshot)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Remove step limit so it can finish.
	m.MaxSteps = 0
	if err := m.Run(); err != nil {
		t.Fatalf("run after restore: %v", err)
	}
	if m.Steps != 107 {
		t.Errorf("total steps = %d, want 107", m.Steps)
	}
}
