package turing

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Direction of head movement on the tape.
type Direction int8

const (
	Left  Direction = -1
	Right Direction = 1
)

func (d Direction) MarshalJSON() ([]byte, error) {
	if d == Left {
		return []byte(`"L"`), nil
	}
	return []byte(`"R"`), nil
}

func (d *Direction) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "L":
		*d = Left
	case "R":
		*d = Right
	default:
		return fmt.Errorf("invalid direction %q: must be L or R", s)
	}
	return nil
}

const (
	// Blank is the symbol that fills the tape initially.
	Blank = byte(0)
	// Halt is the conventional name for the halting state.
	Halt = "HALT"
)

// Rule encodes a single transition: when in State reading Read, write Write,
// move the head in direction Move, and transition to Next.
type Rule struct {
	State string    `json:"state"`
	Read  byte      `json:"read"`
	Write byte      `json:"write"`
	Move  Direction `json:"move"`
	Next  string    `json:"next"`
}

// Program is a complete Turing machine definition.
type Program struct {
	Name       string `json:"name"`
	StartState string `json:"start_state"`
	Rules      []Rule `json:"rules"`
}

// ruleKey is the lookup key into the transition table.
type ruleKey struct {
	state string
	read  byte
}

// Machine is the runtime state of a Turing machine executing a Program.
type Machine struct {
	tape    map[int]byte
	head    int
	state   string
	rules   map[ruleKey]Rule
	program *Program

	Steps    int64 // total steps executed
	MaxSteps int64 // 0 means unlimited
}

// New creates a Machine ready to run the given program.
func New(p *Program, maxSteps int64) (*Machine, error) {
	if p.StartState == "" {
		return nil, errors.New("program has no start state")
	}
	index := make(map[ruleKey]Rule, len(p.Rules))
	for _, r := range p.Rules {
		k := ruleKey{r.State, r.Read}
		if _, dup := index[k]; dup {
			return nil, fmt.Errorf("duplicate rule for state %q read %d", r.State, r.Read)
		}
		index[k] = r
	}
	return &Machine{
		tape:     make(map[int]byte),
		state:    p.StartState,
		rules:    index,
		program:  p,
		MaxSteps: maxSteps,
	}, nil
}

// Halted reports whether the machine is in the halt state.
func (m *Machine) Halted() bool {
	return m.state == Halt
}

// Step executes a single transition and returns true if the machine is still
// running after the step. It returns false when halted or when MaxSteps is
// reached.
func (m *Machine) Step() (bool, error) {
	if m.Halted() {
		return false, nil
	}
	if m.MaxSteps > 0 && m.Steps >= m.MaxSteps {
		return false, fmt.Errorf("step limit %d reached without halting", m.MaxSteps)
	}

	sym := m.tape[m.head]
	rule, ok := m.rules[ruleKey{m.state, sym}]
	if !ok {
		return false, fmt.Errorf("no rule for state %q symbol %d", m.state, sym)
	}

	if rule.Write == Blank {
		delete(m.tape, m.head) // keep tape sparse
	} else {
		m.tape[m.head] = rule.Write
	}
	m.head += int(rule.Move)
	m.state = rule.Next
	m.Steps++
	return !m.Halted(), nil
}

// Run executes the machine until it halts or hits MaxSteps.
func (m *Machine) Run() error {
	for {
		running, err := m.Step()
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
	}
}

// HeadPosition returns the current tape head position.
func (m *Machine) HeadPosition() int { return m.head }

// State returns the current machine state.
func (m *Machine) State() string { return m.state }

// Read returns the symbol under the head.
func (m *Machine) Read() byte { return m.tape[m.head] }

// TapeSlice returns the tape contents from position lo to hi (inclusive),
// using Blank for unwritten cells.
func (m *Machine) TapeSlice(lo, hi int) []byte {
	out := make([]byte, hi-lo+1)
	for i := range out {
		out[i] = m.tape[lo+i]
	}
	return out
}

// TapeStats returns the leftmost and rightmost written positions and the count
// of non-blank cells.
func (m *Machine) TapeStats() (lo, hi int, nonBlank int) {
	first := true
	for pos := range m.tape {
		nonBlank++
		if first || pos < lo {
			lo = pos
		}
		if first || pos > hi {
			hi = pos
		}
		first = false
	}
	return lo, hi, nonBlank
}

// Snapshot is a JSON-serializable point-in-time snapshot of the machine state.
type Snapshot struct {
	Program  *Program       `json:"program"`
	Tape     map[int]byte   `json:"tape"`
	Head     int            `json:"head"`
	State    string         `json:"state"`
	Steps    int64          `json:"steps"`
	MaxSteps int64          `json:"max_steps"`
}

// Snapshot captures the full machine state for serialization or checkpointing.
func (m *Machine) Snapshot() *Snapshot {
	tape := make(map[int]byte, len(m.tape))
	for k, v := range m.tape {
		tape[k] = v
	}
	return &Snapshot{
		Program:  m.program,
		Tape:     tape,
		Head:     m.head,
		State:    m.state,
		Steps:    m.Steps,
		MaxSteps: m.MaxSteps,
	}
}

// Restore creates a Machine from a snapshot, resuming where it left off.
func Restore(s *Snapshot) (*Machine, error) {
	m, err := New(s.Program, s.MaxSteps)
	if err != nil {
		return nil, err
	}
	m.tape = s.Tape
	m.head = s.Head
	m.state = s.State
	m.Steps = s.Steps
	return m, nil
}

// PrintTape renders a human-readable view of the tape centred on the head.
// width is the number of cells to show on each side of the head.
func (m *Machine) PrintTape(width int) string {
	lo := m.head - width
	hi := m.head + width
	cells := m.TapeSlice(lo, hi)

	var sb strings.Builder
	for i, c := range cells {
		pos := lo + i
		if pos == m.head {
			sb.WriteString(fmt.Sprintf("[%d]", c))
		} else {
			sb.WriteString(fmt.Sprintf(" %d ", c))
		}
	}
	return sb.String()
}
