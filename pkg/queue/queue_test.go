package queue

import (
	"encoding/json"
	"testing"

	"github.com/russellwallace/turing-cluster/pkg/search"
	"github.com/russellwallace/turing-cluster/pkg/turing"
)

// The queue serialises Batch and Outcome as JSON on the stream. These round-trip
// tests guard that schema — in particular that a champion Program survives with
// its custom L/R direction encoding — without needing a live Redis.
func TestBatchRoundTrip(t *testing.T) {
	in := search.Batch{Start: 12345, Count: 500, States: 2}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out search.Batch
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round-trip: got %+v, want %+v", out, in)
	}
}

func TestOutcomeRoundTrip(t *testing.T) {
	var tally search.Tally
	tally.Add(turing.BusyBeaver2, true, 6, 4)
	tally.Add(turing.BusyBeaver2, false, 0, 0)
	in := Outcome{Batch: search.Batch{Start: 0, Count: 500, States: 2}, Tally: tally}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Outcome
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	if out.Batch != in.Batch {
		t.Errorf("batch: got %+v, want %+v", out.Batch, in.Batch)
	}
	if out.Tally.Total != 2 || out.Tally.Halted != 1 {
		t.Errorf("counts: Total=%d Halted=%d, want 2 and 1", out.Tally.Total, out.Tally.Halted)
	}
	if out.Tally.StepChampion.Steps != 6 || out.Tally.OnesChampion.Ones != 4 {
		t.Errorf("champions: steps=%d ones=%d, want 6 and 4", out.Tally.StepChampion.Steps, out.Tally.OnesChampion.Ones)
	}
	// The champion program must survive intact, including L/R directions.
	if got, want := len(out.Tally.StepChampion.Program.Rules), len(turing.BusyBeaver2.Rules); got != want {
		t.Fatalf("champion program has %d rules, want %d", got, want)
	}
	for i, r := range out.Tally.StepChampion.Program.Rules {
		if r != turing.BusyBeaver2.Rules[i] {
			t.Errorf("rule %d: got %+v, want %+v", i, r, turing.BusyBeaver2.Rules[i])
		}
	}
}
