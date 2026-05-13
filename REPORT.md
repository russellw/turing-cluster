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

1. **Builder** — `golang:1.22-alpine`; compiles with `CGO_ENABLED=0` and
   `-trimpath` for a fully static, reproducible binary.
2. **Runtime** — `scratch` (empty base image); contains only the binary.
   Resulting image is approximately 7 MB with no OS attack surface.

---

### 4. Kubernetes Manifests (`deploy/`)

| File | Resource | Purpose |
|------|----------|---------|
| `namespace.yaml` | Namespace `turing-cluster` | Isolates all resources |
| `worker-deployment.yaml` | Deployment | Runs 3 worker replicas with rolling updates |
| `worker-service.yaml` | Service (ClusterIP) | Internal DNS name for workers |
| `worker-hpa.yaml` | HorizontalPodAutoscaler | Scales 2–10 pods on CPU utilisation |
| `kustomization.yaml` | Kustomize root | `kubectl apply -k deploy/` deploys everything |

**Deployment configuration:**
- Rolling update: max 1 unavailable, max 1 surge — zero downtime deploys.
- `terminationGracePeriodSeconds: 35` — gives the server's 30-second shutdown
  window room to complete in-flight requests before the pod is killed.
- Resource requests: 500m CPU / 64 Mi memory per pod.
- Resource limits: 2 CPU / 256 Mi memory per pod.
- Liveness probe: `/healthz` every 10s, failure threshold 3.
- Readiness probe: `/healthz` every 5s, failure threshold 2 (pod pulled from
  service rotation faster than it is killed).

**HPA configuration:**
- Target: 70% average CPU utilisation (workers pin a core while running a
  machine, so CPU is a reliable signal).
- Scale-up: add up to 2 pods per minute, stabilisation window 30s.
- Scale-down: remove 1 pod per 2 minutes, stabilisation window 5 minutes —
  conservative, to protect long-running machines that are mid-execution.

**Security hardening (all containers):**
- `runAsNonRoot: true`, `runAsUser: 65534` (nobody).
- `readOnlyRootFilesystem: true`.
- `allowPrivilegeEscalation: false`.
- All Linux capabilities dropped.
- `seccompProfile: RuntimeDefault`.

---

## How to Run

### Locally

```bash
# Run the CLI emulator
go run ./cmd/emulator -program=bb4

# Run with step-by-step trace
go run ./cmd/emulator -program=bb2 -verbose

# Run the HTTP server
go run ./cmd/server -addr :8080

# POST a job
curl -X POST http://localhost:8080/run \
  -H 'Content-Type: application/json' \
  -d '{"program":{"name":"bb4","start_state":"A","rules":[...]},"max_steps":0}'
```

### In Kubernetes

```bash
# Build and push image
docker build -t ghcr.io/russellwallace/turing-cluster/worker:latest .
docker push ghcr.io/russellwallace/turing-cluster/worker:latest

# Deploy
kubectl apply -k deploy/

# Watch pods come up
kubectl get pods -n turing-cluster -w

# Send a job to the service
kubectl run -it --rm curl --image=curlimages/curl --restart=Never -n turing-cluster -- \
  curl -X POST http://turing-worker/run -H 'Content-Type: application/json' -d '{...}'
```

---

## Next Steps

1. **Coordinator** — a pod that accepts a Busy Beaver search specification
   (number of states/symbols, search range), fans out candidate machines to
   workers via `POST /run` with a step limit, collects snapshots, and either
   records halting machines or re-queues non-halted ones for more steps.

2. **Work queue** — replace direct HTTP fan-out with a queue (e.g. Redis
   Streams or Kubernetes Jobs) so the coordinator is not a bottleneck and
   workers can be scaled independently of job submission rate.

3. **Persistent storage** — write halting machine records and high-water-mark
   snapshots to a database so a cluster restart does not lose progress.

4. **CI/CD** — GitHub Actions workflow to build, test, and push the Docker
   image on every push to `main`, and update the image tag in the Deployment.

5. **Observability** — Prometheus metrics endpoint (`/metrics`) exposing steps
   per second, jobs completed, and queue depth; Grafana dashboard.
