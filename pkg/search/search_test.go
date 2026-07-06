package search

import (
	"encoding/json"
	"testing"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func TestSpaceSize(t *testing.T) {
	cases := []struct {
		states int
		want   uint64
	}{
		{1, 64},         // k=8,  cells=2 -> 8^2
		{2, 20736},      // k=12, cells=4 -> 12^4
		{3, 16777216},   // k=16, cells=6 -> 16^6
	}
	for _, c := range cases {
		if got := SpaceSize(c.states); got != c.want {
			t.Errorf("SpaceSize(%d) = %d, want %d", c.states, got, c.want)
		}
		if got := SpaceSizeFloat(c.states); got != float64(c.want) {
			t.Errorf("SpaceSizeFloat(%d) = %v, want %d", c.states, got, c.want)
		}
	}
}

// The full n=2 enumeration must yield exactly 20,736 machines, each a valid
// (loadable, complete) 4-rule table, with no duplicates — the invariant the
// coordinator relied on before this logic moved here.
func TestProgramsCompleteDistinctValid(t *testing.T) {
	total := SpaceSize(2)
	seen := make(map[string]bool, total)
	count := 0
	Programs(2, 0, total, func(_ uint64, p *turing.Program) bool {
		count++
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
		return true
	})
	if count != int(total) {
		t.Errorf("enumerated %d programs, want %d", count, total)
	}
}

// Running every n=2 machine in-process (no HTTP) must reproduce the known busy
// beaver values. This is the end-to-end proof that Expand/Programs decode the
// correct machines.
func TestFullN2SearchFindsBusyBeaver2(t *testing.T) {
	var tally Tally
	Programs(2, 0, SpaceSize(2), func(_ uint64, p *turing.Program) bool {
		m, err := turing.New(p, 1000)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_ = m.Run() // step-limit error is expected for non-halters
		tally.Add(p, m.Halted(), m.Snapshot().Steps, CountOnes(m.Snapshot().Tape))
		return true
	})

	if tally.Total != 20736 {
		t.Errorf("Total = %d, want 20736", tally.Total)
	}
	if tally.Halted == 0 {
		t.Error("no halting machines found")
	}
	if tally.StepChampion.Steps != 6 {
		t.Errorf("S(2) = %d, want 6", tally.StepChampion.Steps)
	}
	if tally.OnesChampion.Ones != 4 {
		t.Errorf("sigma(2) = %d, want 4", tally.OnesChampion.Ones)
	}
}

func TestProgramAtMatchesPrograms(t *testing.T) {
	want := make(map[uint64]string)
	Programs(2, 0, 50, func(i uint64, p *turing.Program) bool {
		b, _ := json.Marshal(p)
		want[i] = string(b)
		return true
	})
	for i := uint64(0); i < 50; i++ {
		b, _ := json.Marshal(ProgramAt(2, i))
		if string(b) != want[i] {
			t.Fatalf("ProgramAt(2, %d) != Programs stream at same index", i)
		}
	}
}

func TestEnumerateCoversSpaceExactly(t *testing.T) {
	const batchSize = 1000
	batches := Enumerate(2, batchSize)

	var next uint64
	var sum uint64
	for _, b := range batches {
		if b.Start != next {
			t.Fatalf("batch starts at %d, want %d (gap or overlap)", b.Start, next)
		}
		if b.Count == 0 || b.Count > batchSize {
			t.Fatalf("batch count %d out of range (0, %d]", b.Count, batchSize)
		}
		if b.States != 2 {
			t.Fatalf("batch States = %d, want 2", b.States)
		}
		next = b.Start + b.Count
		sum += b.Count
	}
	if sum != SpaceSize(2) {
		t.Errorf("batches cover %d machines, want %d", sum, SpaceSize(2))
	}
}

// Expanding every batch must reproduce the same machine sequence as one flat
// Programs call over the whole space.
func TestExpandBatchesMatchFullRange(t *testing.T) {
	var flat []string
	Programs(2, 0, SpaceSize(2), func(_ uint64, p *turing.Program) bool {
		b, _ := json.Marshal(p.Rules)
		flat = append(flat, string(b))
		return true
	})

	var batched []string
	for _, batch := range Enumerate(2, 777) {
		Expand(batch, func(_ uint64, p *turing.Program) bool {
			b, _ := json.Marshal(p.Rules)
			batched = append(batched, string(b))
			return true
		})
	}

	if len(flat) != len(batched) {
		t.Fatalf("batched produced %d programs, flat produced %d", len(batched), len(flat))
	}
	for i := range flat {
		if flat[i] != batched[i] {
			t.Fatalf("program %d differs between flat and batched enumeration", i)
		}
	}
}

func TestTallyFoldsByMax(t *testing.T) {
	var tally Tally
	p := turing.BusyBeaver2
	tally.Add(p, true, 6, 4)
	tally.Add(p, true, 3, 2) // smaller — must not displace champions
	tally.Add(p, false, 999, 999)
	tally.Add(p, true, 6, 4) // duplicate of the champion — idempotent

	if tally.Total != 4 {
		t.Errorf("Total = %d, want 4", tally.Total)
	}
	if tally.Halted != 3 {
		t.Errorf("Halted = %d, want 3", tally.Halted)
	}
	if tally.StepChampion.Steps != 6 {
		t.Errorf("StepChampion.Steps = %d, want 6", tally.StepChampion.Steps)
	}
	if tally.OnesChampion.Ones != 4 {
		t.Errorf("OnesChampion.Ones = %d, want 4", tally.OnesChampion.Ones)
	}
}

func TestMergeCombinesTallies(t *testing.T) {
	var a, b Tally
	a.Add(turing.BusyBeaver2, true, 6, 4)
	b.Add(turing.BusyBeaver2, true, 3, 7)
	a.Merge(b)

	if a.Total != 2 || a.Halted != 2 {
		t.Errorf("after merge Total=%d Halted=%d, want 2 and 2", a.Total, a.Halted)
	}
	if a.StepChampion.Steps != 6 {
		t.Errorf("merged StepChampion.Steps = %d, want 6", a.StepChampion.Steps)
	}
	if a.OnesChampion.Ones != 7 {
		t.Errorf("merged OnesChampion.Ones = %d, want 7", a.OnesChampion.Ones)
	}
}
