package turing

// BusyBeaver2 is the 2-state 2-symbol busy beaver: halts in 6 steps, writes 4 ones.
var BusyBeaver2 = &Program{
	Name:       "busy-beaver-2",
	StartState: "A",
	Rules: []Rule{
		{State: "A", Read: 0, Write: 1, Move: Right, Next: "B"},
		{State: "A", Read: 1, Write: 1, Move: Left, Next: "B"},
		{State: "B", Read: 0, Write: 1, Move: Left, Next: "A"},
		{State: "B", Read: 1, Write: 1, Move: Right, Next: Halt},
	},
}

// BusyBeaver3 is the 3-state 2-symbol busy beaver champion for ones written (sigma(3) = 6):
// halts in 14 steps, writes 6 ones.
var BusyBeaver3 = &Program{
	Name:       "busy-beaver-3",
	StartState: "A",
	Rules: []Rule{
		{State: "A", Read: 0, Write: 1, Move: Right, Next: "B"},
		{State: "A", Read: 1, Write: 1, Move: Left, Next: Halt},
		{State: "B", Read: 0, Write: 0, Move: Right, Next: "C"},
		{State: "B", Read: 1, Write: 1, Move: Right, Next: "B"},
		{State: "C", Read: 0, Write: 1, Move: Left, Next: "C"},
		{State: "C", Read: 1, Write: 1, Move: Left, Next: "A"},
	},
}

// BusyBeaver4 is the 4-state 2-symbol busy beaver: halts in 107 steps, writes 13 ones.
var BusyBeaver4 = &Program{
	Name:       "busy-beaver-4",
	StartState: "A",
	Rules: []Rule{
		{State: "A", Read: 0, Write: 1, Move: Right, Next: "B"},
		{State: "A", Read: 1, Write: 1, Move: Left, Next: "B"},
		{State: "B", Read: 0, Write: 1, Move: Left, Next: "A"},
		{State: "B", Read: 1, Write: 0, Move: Left, Next: "C"},
		{State: "C", Read: 0, Write: 1, Move: Right, Next: Halt},
		{State: "C", Read: 1, Write: 1, Move: Left, Next: "D"},
		{State: "D", Read: 0, Write: 1, Move: Right, Next: "D"},
		{State: "D", Read: 1, Write: 0, Move: Right, Next: "A"},
	},
}

// BusyBeaver5 is the 5-state 2-symbol busy beaver champion: halts in 47,176,870 steps,
// writes 4098 ones. This will run for a while — set an appropriate MaxSteps.
var BusyBeaver5 = &Program{
	Name:       "busy-beaver-5",
	StartState: "A",
	Rules: []Rule{
		{State: "A", Read: 0, Write: 1, Move: Right, Next: "B"},
		{State: "A", Read: 1, Write: 1, Move: Left, Next: "C"},
		{State: "B", Read: 0, Write: 1, Move: Right, Next: "C"},
		{State: "B", Read: 1, Write: 1, Move: Right, Next: "B"},
		{State: "C", Read: 0, Write: 1, Move: Right, Next: "D"},
		{State: "C", Read: 1, Write: 0, Move: Left, Next: "E"},
		{State: "D", Read: 0, Write: 1, Move: Left, Next: "A"},
		{State: "D", Read: 1, Write: 1, Move: Left, Next: "D"},
		{State: "E", Read: 0, Write: 1, Move: Right, Next: Halt},
		{State: "E", Read: 1, Write: 0, Move: Left, Next: "A"},
	},
}

// Incrementer adds 1 to a unary number encoded as a run of 1s.
var Incrementer = &Program{
	Name:       "incrementer",
	StartState: "A",
	Rules: []Rule{
		{State: "A", Read: 1, Write: 1, Move: Right, Next: "A"},
		{State: "A", Read: 0, Write: 1, Move: Right, Next: Halt},
	},
}
