package turing_test

import (
	"encoding/json"
	"testing"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func runAndCheck(t *testing.T, p *turing.Program, wantSteps int64, wantOnes int) {
	t.Helper()
	m, err := turing.New(p, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.Steps != wantSteps {
		t.Errorf("steps = %d, want %d", m.Steps, wantSteps)
	}
	_, _, ones := m.TapeStats()
	if ones != wantOnes {
		t.Errorf("ones on tape = %d, want %d", ones, wantOnes)
	}
}

func TestBusyBeaver2(t *testing.T) { runAndCheck(t, turing.BusyBeaver2, 6, 4) }
func TestBusyBeaver3(t *testing.T) { runAndCheck(t, turing.BusyBeaver3, 14, 6) }
func TestBusyBeaver4(t *testing.T) { runAndCheck(t, turing.BusyBeaver4, 107, 13) }

func TestBusyBeaver5(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping BB5 in short mode (47M steps)")
	}
	runAndCheck(t, turing.BusyBeaver5, 47_176_870, 4098)
}

func TestIncrementer(t *testing.T) {
	// Tape starts blank; machine should write one 1, giving unary 1.
	m, err := turing.New(turing.Incrementer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	_, _, ones := m.TapeStats()
	if ones != 1 {
		t.Errorf("incrementer: ones = %d, want 1", ones)
	}
}

func TestStepLimit(t *testing.T) {
	m, err := turing.New(turing.BusyBeaver4, 50)
	if err != nil {
		t.Fatal(err)
	}
	err = m.Run()
	if err == nil {
		t.Fatal("expected step-limit error, got nil")
	}
	if m.Steps != 50 {
		t.Errorf("steps = %d, want 50", m.Steps)
	}
}

func TestSnapshot(t *testing.T) {
	m, err := turing.New(turing.BusyBeaver3, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Run halfway.
	for range 10 {
		if _, err := m.Step(); err != nil {
			t.Fatal(err)
		}
	}

	snap := m.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	var snap2 turing.Snapshot
	if err := json.Unmarshal(data, &snap2); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	m2, err := turing.Restore(&snap2)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Both machines should finish with the same result.
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	if err := m2.Run(); err != nil {
		t.Fatal(err)
	}
	if m.Steps != m2.Steps {
		t.Errorf("after restore: steps %d != %d", m.Steps, m2.Steps)
	}
}

func TestDuplicateRule(t *testing.T) {
	p := &turing.Program{
		Name:       "bad",
		StartState: "A",
		Rules: []turing.Rule{
			{State: "A", Read: 0, Write: 1, Move: turing.Right, Next: turing.Halt},
			{State: "A", Read: 0, Write: 0, Move: turing.Left, Next: turing.Halt},
		},
	}
	_, err := turing.New(p, 0)
	if err == nil {
		t.Error("expected error for duplicate rule, got nil")
	}
}

func TestMissingRule(t *testing.T) {
	p := &turing.Program{
		Name:       "incomplete",
		StartState: "A",
		Rules: []turing.Rule{
			// Only handles symbol 1, not 0.
			{State: "A", Read: 1, Write: 1, Move: turing.Right, Next: turing.Halt},
		},
	}
	m, err := turing.New(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Tape starts with 0, so this should error immediately.
	_, err = m.Step()
	if err == nil {
		t.Error("expected error for missing rule, got nil")
	}
}
