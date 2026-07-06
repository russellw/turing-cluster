# Turing Cluster — Progress Report

## Goal

Build a Turing machine emulator that runs in a Kubernetes cluster, with
industrial-grade operational characteristics. The motivating use case is
distributing Busy Beaver searches across a fleet of machines.

---

## What Has Been Built

### 1. Turing Machine Emulator (`pkg/turing`)

A correct, efficient 2-symbol Turing machine emulator in Go.

**Tape** — implemented as a sparse `map[int]byte`, giving an infinite tape
in both directions without pre-allocation. Long-running machines (e.g. BB5,
which spans positions −12,243 to +45) use only the memory they actually
touch.

**Transition table** — rules are indexed at construction time into a hash map
keyed by `(state, symbol)`, giving O(1) lookups per step. Duplicate rules are
detected at load time and rejected with an error.

**Step control** — the `Step()` / `Run()` / `MaxSteps` API allows callers to
run a machine to completion, run it for exactly N steps, or impose a hard
limit. This is the foundation for partitioning work across cluster nodes.

**Snapshots** — `Snapshot()` serialises the complete machine state (tape,
head position, current state, step count, program, step limit) to a
JSON-compatible struct. `Restore()` reconstructs a live machine from a
snapshot. This enables checkpointing and hand-off between workers.

**Built-in programs:**

| Program | States | Steps to halt | Ones written |
|---------|--------|---------------|--------------|
| BB2     | 2      | 6             | 4            |
| BB3     | 3      | 14            | 6 (σ(3))     |
| BB4     | 4      | 107           | 13           |
| BB5     | 5      | 47,176,870    | 4,098        |
| Incrementer | 1  | 2             | 1            |

All values are verified by the test suite. BB5 runs in ~7 seconds at roughly
7 million steps per second on a single core.

**Test coverage** — `machine_test.go` covers: correct halt values for all
four busy beaver machines, step limiting, snapshot round-trip (serialise at
step 10, restore, complete — same result), duplicate rule rejection, and
missing rule error.

---

### 2. HTTP Worker Server (`cmd/server`)

Wraps the emulator in an HTTP server so it can run as a Kubernetes pod and
receive work over the network.

**`POST /run`**

Request:
```json
{
  "program": { "name": "...", "start_state": "A", "rules": [...] },
  "max_steps": 1000000
}
```

Response:
```json
{
  "snapshot": { "tape": {...}, "head": 0, "state": "HALT", "steps": 107, ... },
  "halted": true,
  "elapsed_ms": 12,
  "error": ""
}
```

The response snapshot can be passed directly to `Restore()` on another worker
to continue execution — this is the primitive that makes distributed
step-partitioned search possible.

**`GET /healthz`** — returns 200 OK; used by Kubernetes liveness and
readiness probes.

**Operational details:**
- Graceful shutdown on SIGTERM (Kubernetes sends this before killing a pod),
  with a 30-second drain window.
- Structured JSON-compatible logging via `log/slog`.
- Listen address configurable via `-addr` flag or `PORT` environment variable.
- Per-request write timeout of 10 minutes (accommodates long machine runs).

---

### 3. Docker Image (`Dockerfile`)

Two-stage build:

1. **Builder** — `golang:1.24-alpine`; compiles with `CGO_ENABLED=0` and
   `-trimpath` for a fully static, reproducible binary. A build `ARG CMD`
   selects the target (`server` or `coordinator`), so one Dockerfile produces
   both images.
2. **Runtime** — `scratch` (empty base image); contains only the binary.
   Resulting image is approximately 16 MB with no OS attack surface.

---

### 4. Kubernetes Manifests (`deploy/`)

| File | Resource | Purpose |
|------|----------|---------|
| `namespace.yaml` | Namespace `turing-cluster` | Isolates all resources |
| `worker-deployment.yaml` | Deployment | Worker pods (rolling updates); also consume the queue |
| `worker-service.yaml` | Service (ClusterIP) | Internal DNS name for workers; fronts `/metrics` |
| `redis/*` | StatefulSet + Service + ConfigMap | Redis queue + champion state, AOF-persisted on a PVC |
| `coordinator-job.yaml` | Job | Run-once search driver (applied on demand) |
| `keda-scaledobject.yaml` | KEDA ScaledObject | Autoscale workers 1–10 on queue lag |
| `monitoring/*` | ServiceMonitors, Pushgateway, dashboard | Prometheus/Grafana wiring |
| `kustomization.yaml` | Kustomize root | `kubectl apply -k deploy/` deploys the core stack |

The KEDA and monitoring manifests depend on operator CRDs installed via Helm, so
they live outside the root kustomize and are applied after those installs. The
CPU HorizontalPodAutoscaler that originally scaled on CPU was **retired** once
work became queue-driven — CPU is no longer a meaningful signal (see §8).

**Deployment configuration:**
- Rolling update: max 1 unavailable, max 1 surge — zero downtime deploys.
- `terminationGracePeriodSeconds: 35` — gives the server's 30-second shutdown
  window room to complete in-flight requests before the pod is killed.
- Resource requests: 500m CPU / 64 Mi memory per pod.
- Resource limits: 2 CPU / 256 Mi memory per pod.
- Liveness probe: `/healthz` every 10s, failure threshold 3.
- Readiness probe: `/healthz` every 5s, failure threshold 2 (pod pulled from
  service rotation faster than it is killed).

**Security hardening (all containers):**
- `runAsNonRoot: true`, `runAsUser: 65534` (nobody).
- `readOnlyRootFilesystem: true`.
- `allowPrivilegeEscalation: false`.
- All Linux capabilities dropped.
- `seccompProfile: RuntimeDefault`.

---

### 5. Coordinator (`cmd/coordinator`)

The first component that makes the workers act as a *cluster*: a brute-force
Busy Beaver search driver. It enumerates every candidate machine, fans them out
to the worker fleet over `POST /run`, and reports the champions.

**Enumeration** — for an n-state, 2-symbol machine each of the `2n`
`(state, symbol)` cells is assigned a transition drawn from an alphabet of
`write × move × next` = `2 · 2 · (n+1)` possibilities. A mixed-radix odometer
walks the full space, streaming complete transition tables. Because every cell
is defined, there are no missing-rule errors; machines that fail to halt simply
hit the step limit and are counted as non-halters. For n=2 the space is
`12⁴ = 20,736` machines.

**Fan-out** — `-concurrency` goroutines pull candidates and round-robin them
across the workers listed in `-workers` (comma-separated base URLs). Each
worker's returned snapshot is triaged: `Steps` gives the running time and the
non-blank tape-cell count gives σ. Transport-level failures abort the search;
machine-level "step limit reached" is expected and ignored.

**Champions** — the search reports S(n) (most steps before halting) and σ(n)
(most 1s left on the tape), plus the winning transition table.

**Safety** — a `-force`-gated threshold refuses search spaces above 10⁷
machines, so the naive full enumeration can't be launched against n≥3 by
accident (n=3 is ~16.7M machines and needs symmetry reduction first).

Verified end-to-end against a live worker: all 20,736 n=2 machines evaluated
(9,784 halting), yielding **S(2) = 6** and **σ(2) = 4** — the known busy beaver
values. `main_test.go` reproduces this against an in-process `httptest` worker.

```bash
# Against a local worker on :8080
go run ./cmd/coordinator -workers=http://localhost:8080 -states=2

# Against the in-cluster Service
go run ./cmd/coordinator -workers=http://turing-worker -states=2
```

This enumerates the *full* transition space with no symmetry reduction, so it
is only practical for very small n. Normal-form / symmetry breaking is the
natural next step before n≥3 becomes feasible.

---

### 6. Distributed Search over a Work Queue (`pkg/search`, `pkg/queue`)

The coordinator no longer has to fan work out over HTTP itself. A Redis Streams
queue decouples it from the workers so they scale independently.

**`pkg/search`** is the enumeration made distributable. It treats the transition
space as a mixed-radix number and exposes it as `Batch{Start, Count, States}`
index ranges. `Enumerate` partitions the space into batches; `Programs`/`Expand`
decode a range into machines. Outcomes fold into a `Tally` (S(n), σ(n), counts)
**by max** — an idempotent operation, which is the property that makes
at-least-once queue delivery safe: reprocessing a redelivered batch can only
recompute the same maxima. Unit tests reproduce the full n=2 space (20,736
distinct machines → S(2)=6, σ(2)=4) purely in-process.

**`pkg/queue`** wraps a Redis client with the domain operations:
- **`jobs` stream** — the coordinator enqueues one message per batch; workers
  consume via the `workers` consumer group (`XREADGROUP`). Batches (not
  individual machines) keep the queue short while per-message work stays
  meaningful.
- **Crash recovery** — `XAUTOCLAIM` reclaims batches a dead worker left pending,
  and a worker acks a job only *after* its outcome is durably on the results
  stream — so no work is silently lost.
- **`results` stream** — workers publish a per-batch outcome; the coordinator
  reads them, folds the global champions, and mirrors high-water marks to the
  **`champions` hash**.

Redis itself runs as a StatefulSet with AOF persistence on a PVC, so queued work
and champion state survive a restart. Verified fully in-cluster (coordinator Job
+ worker fleet + Redis): S(2)=6, σ(2)=4 with zero pending jobs at the end.

---

### 7. Observability (`/metrics`, Prometheus, Grafana)

Every worker exposes Prometheus `/metrics` on its HTTP port: `turing_steps_total`
(rate → steps/sec), `turing_candidates_total`, `turing_halts_total`, a
`turing_batch_duration_seconds` histogram, and a `turing_worker_busy` gauge. A
**ServiceMonitor** tells the Prometheus Operator to scrape them.

The coordinator is a run-once Job, so its own endpoint dies with it; it therefore
**pushes** its final summary (champions, batches, backlog) to a **Pushgateway** —
the standard pattern for batch-job metrics — which is scraped continuously. The
observability stack is **kube-prometheus-stack** (Prometheus + Grafana +
operator); a Grafana dashboard ships as a ConfigMap and is auto-imported by the
sidecar. Verified: all scrape targets healthy, champions queryable in Prometheus,
dashboard imported at `/d/turing-cluster/`.

---

### 8. Autoscaling on Queue Lag (KEDA)

The original CPU HorizontalPodAutoscaler was **retired**: once work is queued,
CPU is no longer the honest signal — a fleet idle between searches reads low CPU
even with a huge backlog waiting. **KEDA** replaces it, scaling the worker
Deployment 1–10 on the Redis stream's *lag* (undelivered entries in the `workers`
group) via its `redis-streams` scaler — KEDA reads Redis directly, so the
autoscaler needs no Prometheus dependency.

Scale-down is now safe without the old conservative window: a terminated worker
finishes its in-flight batch (graceful drain) and any undelivered/unacked work
stays on the stream to be reclaimed. Verified on a live 500k-entry backlog —
workers scaled **1 → 5 → 10** as KEDA saw the lag, then back to **1** once drained.

The full design and per-phase verification live in
**[docs/DESIGN-queue-observability.md](docs/DESIGN-queue-observability.md)**.

---

## How to Run

See **[README.md](README.md)** for local and Kubernetes quick-start commands.
One additional low-level example — POSTing a raw job to a worker:

```bash
curl -X POST http://localhost:8080/run \
  -H 'Content-Type: application/json' \
  -d '{"program":{"name":"bb4","start_state":"A","rules":[...]},"max_steps":0}'
```

---

## Next Steps

**Done since the first cut:**
- ~~**Work queue**~~ — Redis Streams decouples the coordinator from the workers,
  with consumer-group delivery and crash reclaim (§6).
- ~~**Observability**~~ — Prometheus `/metrics`, ServiceMonitor scraping, a
  Pushgateway for the run-once coordinator, and a Grafana dashboard (§7).
- ~~**Autoscaling**~~ — KEDA scales workers on queue lag; the CPU HPA is retired
  (§8).
- ~~**Partial persistence**~~ — Redis (AOF on a PVC) already survives a restart
  with queued work and champion state intact.

**Still open:**
1. **Symmetry-reduced enumeration** — normal-form / symmetry breaking so n≥3
   (~16.7M machines) becomes feasible; also resumable search state and re-queuing
   non-halted candidates for more steps rather than discarding them.

2. **Durable champion records** — write halting-machine records to a database
   (not just the Redis `champions` hash) for a permanent, queryable history.

3. **CI/CD** — GitHub Actions to build, test, and push the images on every push
   to `main`, and bump the image tag in the Deployment.
