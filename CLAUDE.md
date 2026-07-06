# CLAUDE.md

Guidance for working on this codebase.

## Purpose

This is a learning project: teach myself Kubernetes by implementing a Turing
machine emulator that runs in a cluster with industrial-grade operational
characteristics. The motivating use case is distributing Turing machine
evaluation across a fleet of machines as part of a search for Busy Beaver
numbers. Its primary secondary purpose is as a **portfolio project** to
demonstrate Kubernetes competence — keep that audience in mind for anything
outward-facing.

## Documentation map

- **README.md** — for outsiders and potential employers. Polished, outward-facing.
- **REPORT.md** — detailed engineering progress report: what's been built, how it
  was verified, and what's next. Everything that doesn't belong in the other two.
- **CLAUDE.md** (this file) — guidance for editing the code.
- **docs/DESIGN-queue-observability.md** — reference design for the queue +
  metrics + autoscaling arc, phased so it can be implemented across sessions.

## Repository layout

| Path | What it is |
|------|------------|
| `pkg/turing/` | Core 2-symbol Turing machine emulator (tape, transitions, snapshots) and built-in Busy Beaver programs |
| `pkg/search/` | Transition-space enumeration as index-range Batches; decode indices→machines; fold outcomes into champions (Tally) |
| `pkg/queue/` | Redis Streams work queue: batch enqueue, consumer-group reads + crash reclaim, outcome publish/collect |
| `cmd/emulator/` | CLI that runs a built-in program locally |
| `cmd/server/` | Worker: HTTP server (`POST /run`, `GET /healthz`, `GET /metrics`) + queue consumer (gated on `REDIS_ADDR`) |
| `cmd/coordinator/` | Busy Beaver search driver — queue mode (`-redis`) or direct HTTP fan-out |
| `deploy/` | K8s manifests. Root kustomize = core stack; `deploy/monitoring/` and `keda-scaledobject.yaml` applied after their Helm add-ons |
| `Dockerfile` | Two-stage build → static binary on `scratch`; `ARG CMD` builds worker or coordinator |

Module: `github.com/russellwallace/turing-cluster` (Go 1.24 — bumped from 1.22
when the Redis client was added for the queue path).

## Common commands

```bash
# Build / test everything
go build ./...
go test ./...

# Run the CLI emulator
go run ./cmd/emulator -program=bb4
go run ./cmd/emulator -program=bb2 -verbose   # step-by-step trace

# Run the HTTP worker locally
go run ./cmd/server -addr :8080

# Run the coordinator against a local worker
go run ./cmd/coordinator -workers=http://localhost:8080 -states=2

# Docker image
docker build -t ghcr.io/russellwallace/turing-cluster/worker:latest .

# Deploy to Kubernetes
kubectl apply -k deploy/
```

## Key design decisions to know before editing

- **2-symbol machines only.** The tape is a sparse `map[int]byte` (infinite in
  both directions, allocates only touched cells).
- **Snapshot/Restore is the distribution primitive.** `Snapshot()` serialises
  complete machine state to a JSON-compatible struct; `Restore()` rebuilds a
  live machine. This is what makes step-partitioned hand-off between workers
  possible — preserve its round-trip fidelity.
- **Transition tables are indexed at construction** into a `(state, symbol)`
  hash map for O(1) steps; duplicate rules are rejected at load time.
- **The coordinator enumerates every cell of the transition space**, so machines
  never hit missing-rule errors — non-halters simply reach the step limit.
- **The full-enumeration search has a `-force`-gated safety threshold** (refuses
  spaces above 10⁷ machines) so naive enumeration can't be launched against
  n≥3 by accident. Symmetry reduction is required before n≥3 is feasible.
- **The server does graceful shutdown on SIGTERM** with a 30s drain window; the
  Deployment's `terminationGracePeriodSeconds` (35s) is deliberately larger.
  Keep those two numbers consistent if you change either.
