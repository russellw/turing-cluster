package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/russellwallace/turing-cluster/pkg/turing"
)

var programs = map[string]*turing.Program{
	"bb2":        turing.BusyBeaver2,
	"bb3":        turing.BusyBeaver3,
	"bb4":        turing.BusyBeaver4,
	"bb5":        turing.BusyBeaver5,
	"incrementer": turing.Incrementer,
}

func main() {
	name := flag.String("program", "bb4", "program to run: bb2, bb3, bb4, bb5, incrementer")
	maxSteps := flag.Int64("max-steps", 0, "step limit (0 = unlimited)")
	verbose := flag.Bool("verbose", false, "print each step")
	snapshot := flag.Bool("snapshot", false, "dump final state as JSON")
	tapeWidth := flag.Int("tape-width", 20, "cells to show on each side of head in verbose mode")
	flag.Parse()

	p, ok := programs[*name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown program %q\n", *name)
		fmt.Fprintf(os.Stderr, "available programs: bb2, bb3, bb4, bb5, incrementer\n")
		os.Exit(1)
	}

	m, err := turing.New(p, *maxSteps)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	fmt.Printf("Running %s\n", p.Name)
	start := time.Now()

	if *verbose {
		for {
			fmt.Printf("step %6d  state=%-6s  head=%4d  tape: %s\n",
				m.Steps, m.State(), m.HeadPosition(), m.PrintTape(*tapeWidth))
			running, err := m.Step()
			if err != nil {
				log.Fatalf("step error: %v", err)
			}
			if !running {
				break
			}
		}
	} else {
		if err := m.Run(); err != nil {
			log.Fatalf("run error: %v", err)
		}
	}

	elapsed := time.Since(start)
	lo, hi, nonBlank := m.TapeStats()
	fmt.Printf("Halted after %d steps in %v\n", m.Steps, elapsed)
	fmt.Printf("Non-blank cells: %d  tape span: [%d, %d]\n", nonBlank, lo, hi)
	if m.Steps > 0 {
		fmt.Printf("Throughput: %.0f steps/sec\n", float64(m.Steps)/elapsed.Seconds())
	}

	if *snapshot {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(m.Snapshot()); err != nil {
			log.Fatalf("snapshot: %v", err)
		}
	}
}
