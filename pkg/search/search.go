// Package search enumerates the space of n-state, 2-symbol Turing machines and
// provides the primitives for distributing that enumeration across a worker
// fleet: contiguous index ranges (Batch), a decoder from index to machine
// (ProgramAt / Programs), and a champion accumulator that folds outcomes by max
// (Tally).
//
// The enumeration is the *full* transition space with no symmetry reduction, so
// it is only practical for very small n. For n=2 the space is 12^4 = 20,736
// machines. This package deliberately says nothing about Turing machines beyond
// what distribution requires; symmetry reduction is a separate concern (see
// docs/DESIGN-queue-observability.md).
//
// # Encoding
//
// An n-state machine has 2n (state, symbol) cells. Each cell is assigned one of
// k = write(2) * move(2) * next(n+1) transitions drawn from Alphabet. A machine
// is therefore a number in [0, k^(2n)): a mixed-radix odometer where cell 0 (the
// start state reading a blank) is the least-significant digit. Cell c encodes
// state c/2 reading symbol c%2, matching the order Alphabet and StateNames
// produce. Index 0 is the machine whose every cell writes 0, moves left, and
// goes to the start state.
package search

import (
	"math"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

// Transition is one entry in the per-cell alphabet of possible moves: write a
// symbol, move the head, and transition to a next state (which may be HALT).
type Transition struct {
	Write byte
	Move  turing.Direction
	Next  string
}

// Batch is a contiguous half-open range [Start, Start+Count) of machine indices
// in the transition space. It is the unit of work handed to a worker: small
// enough to keep the queue short, large enough that per-message overhead is
// negligible.
type Batch struct {
	Start uint64 `json:"start"`
	Count uint64 `json:"count"`
	// States is carried so a worker can expand the batch without out-of-band
	// context about which space it belongs to.
	States int `json:"states"`
}

// Champion records the best machine found for a metric.
type Champion struct {
	Program *turing.Program
	Steps   int64
	Ones    int
}

// StateNames returns the state names A, B, C, ... for an n-state machine.
func StateNames(states int) []string {
	names := make([]string, states)
	for i := range names {
		names[i] = string(rune('A' + i))
	}
	return names
}

// Alphabet returns the k = 2*2*(states+1) transitions a single cell may take:
// write 0 or 1, move left or right, and go to any state or HALT. The order is
// fixed and is what ProgramAt decodes against — do not reorder without a
// matching change there.
func Alphabet(states int) []Transition {
	nexts := append(append([]string{}, StateNames(states)...), turing.Halt)
	var a []Transition
	for _, w := range []byte{0, 1} {
		for _, mv := range []turing.Direction{turing.Left, turing.Right} {
			for _, nx := range nexts {
				a = append(a, Transition{Write: w, Move: mv, Next: nx})
			}
		}
	}
	return a
}

// CellCount returns the number of (state, symbol) cells: 2*states.
func CellCount(states int) int { return 2 * states }

// SpaceSize returns the number of machines in the n-state space, k^(2n). It
// overflows uint64 for large n, but the full space is only ever enumerated for
// tiny n (n=3 is already ~16.7M and needs symmetry reduction first), so this is
// exact where it matters.
func SpaceSize(states int) uint64 {
	k := uint64(2 * 2 * (states + 1))
	n := uint64(1)
	for i := 0; i < 2*states; i++ {
		n *= k
	}
	return n
}

// SpaceSizeFloat is SpaceSize as a float64, for safety-threshold comparisons
// where an approximate magnitude is all that's needed and overflow must not
// wrap silently.
func SpaceSizeFloat(states int) float64 {
	return math.Pow(float64(2*2*(states+1)), float64(2*states))
}

// Enumerate partitions the whole n-state space into batches of at most
// batchSize machines, covering [0, SpaceSize(states)) exactly once with no gaps
// or overlaps.
func Enumerate(states int, batchSize uint64) []Batch {
	if batchSize == 0 {
		batchSize = 1
	}
	total := SpaceSize(states)
	var batches []Batch
	for start := uint64(0); start < total; start += batchSize {
		count := batchSize
		if start+count > total {
			count = total - start
		}
		batches = append(batches, Batch{Start: start, Count: count, States: states})
	}
	return batches
}

// ProgramAt decodes the machine at the given index in the n-state space. It
// rebuilds the alphabet on each call; use Programs to expand many indices.
func ProgramAt(states int, index uint64) *turing.Program {
	names := StateNames(states)
	alpha := Alphabet(states)
	return decode(states, names, alpha, index)
}

// Programs calls fn for each machine index in [start, start+count), decoding the
// alphabet once for the whole range. If fn returns false, enumeration stops.
func Programs(states int, start, count uint64, fn func(index uint64, p *turing.Program) bool) {
	names := StateNames(states)
	alpha := Alphabet(states)
	for i := uint64(0); i < count; i++ {
		index := start + i
		if !fn(index, decode(states, names, alpha, index)) {
			return
		}
	}
}

// Expand is Programs over a Batch's range.
func Expand(b Batch, fn func(index uint64, p *turing.Program) bool) {
	Programs(b.States, b.Start, b.Count, fn)
}

// decode materialises the machine at index given a precomputed state-name list
// and alphabet. Cell c is the (index / k^c) % k digit of the mixed-radix number.
func decode(states int, names []string, alpha []Transition, index uint64) *turing.Program {
	k := uint64(len(alpha))
	cells := 2 * states
	rules := make([]turing.Rule, cells)
	idx := index
	for cell := 0; cell < cells; cell++ {
		t := alpha[idx%k]
		idx /= k
		rules[cell] = turing.Rule{
			State: names[cell/2],
			Read:  byte(cell % 2),
			Write: t.Write,
			Move:  t.Move,
			Next:  t.Next,
		}
	}
	return &turing.Program{
		Name:       "candidate",
		StartState: names[0],
		Rules:      rules,
	}
}

// CountOnes counts the non-blank cells on a snapshot tape. In a 2-symbol machine
// every non-blank cell holds a 1, so this is sigma for that machine.
func CountOnes(tape map[int]byte) int {
	n := 0
	for _, v := range tape {
		if v != turing.Blank {
			n++
		}
	}
	return n
}

// Tally accumulates search outcomes, folding them into the step champion S(n)
// (most steps before halting) and the ones champion sigma(n) (most 1s left on
// the tape). Because both are maxima, folding is idempotent: observing the same
// machine twice never changes the result, which is what makes at-least-once
// delivery safe both locally in a worker and globally in the coordinator.
type Tally struct {
	Total  int // machines observed
	Halted int // machines that halted within the step limit

	StepChampion Champion // most steps before halting
	OnesChampion Champion // most 1s left on the tape
}

// Add folds one machine outcome into the tally.
func (t *Tally) Add(prog *turing.Program, halted bool, steps int64, ones int) {
	t.Total++
	if !halted {
		return
	}
	t.Halted++
	if steps > t.StepChampion.Steps {
		t.StepChampion = Champion{Program: prog, Steps: steps, Ones: ones}
	}
	if ones > t.OnesChampion.Ones {
		t.OnesChampion = Champion{Program: prog, Steps: steps, Ones: ones}
	}
}

// Merge folds another tally into this one — used to combine per-worker local
// tallies into a global result.
func (t *Tally) Merge(o Tally) {
	t.Total += o.Total
	t.Halted += o.Halted
	if o.StepChampion.Steps > t.StepChampion.Steps {
		t.StepChampion = o.StepChampion
	}
	if o.OnesChampion.Ones > t.OnesChampion.Ones {
		t.OnesChampion = o.OnesChampion
	}
}
