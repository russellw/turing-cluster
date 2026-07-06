# turing-cluster

Distributed Turing machine evaluation on Kubernetes — with production-grade
operational characteristics.

A Turing machine emulator, HTTP workers that run as Kubernetes pods, and a
coordinator that distributes work across a fleet of workers to search for
[Busy Beaver](https://en.wikipedia.org/wiki/Busy_beaver) numbers. The emulator
is the excuse; the point is the operational engineering around it — a
queue-decoupled architecture, autoscaling on real backlog, an observability
stack, health probing, graceful lifecycle handling, and zero-downtime deploys.

## Architecture

Workers and the coordinator are decoupled by a Redis Streams work queue. The
coordinator (a run-once Job) enqueues *batches* of candidate machines; workers
consume them, run each locally, and publish per-batch outcomes that the
coordinator folds into the Busy Beaver champions. KEDA scales the workers on the
queue's backlog; Prometheus and Grafana watch the whole thing.

```
                 enqueue batches                          consume (XREADGROUP)
  coordinator ───────────────────►   Redis Streams   ◄─────────────────  worker pods
  (run-once Job)  ◄───────────────   jobs · results         (KEDA: 1–10 replicas,
       │            fold outcomes    champions hash          scaled on queue lag)
       │                                   ▲                          │
       └─ push summary ─► Pushgateway      │ lag                      │ /metrics
                                │          └───── KEDA ───────────────┤
                                └──────► Prometheus ◄─── scrape ───────┘
                                              │
                                           Grafana
```

- **`pkg/turing`** — a correct, fast 2-symbol Turing machine emulator. Sparse
  infinite tape, O(1) transitions, and full state **snapshot/restore** so a
  machine's execution can be checkpointed and handed between workers.
- **`pkg/search`** — enumerates the transition space as index ranges (batches),
  decodes indices to machines, and folds outcomes into champions by max (which
  makes at-least-once queue delivery safe).
- **`pkg/queue`** — the Redis Streams work queue: batch enqueue, consumer-group
  reads with reclaim of crashed workers' jobs, outcome publish/collect.
- **`cmd/server`** — wraps the emulator in an HTTP server (`POST /run`,
  `GET /healthz`, `GET /metrics`) *and* consumes batch jobs from the queue.
- **`cmd/coordinator`** — enumerates candidates and distributes them (queue or
  direct HTTP fan-out), reporting the Busy Beaver champions.
- **`cmd/emulator`** — a CLI for running built-in programs locally.

## The Kubernetes & operations story

This is where the project earns its keep:

- **Decoupled, scalable architecture** — a Redis Streams queue sits between the
  producer and the workers, so they scale independently and a crashed worker's
  in-flight batch is automatically reclaimed. Redis runs as a **StatefulSet** with
  a persistent volume and AOF, so queued work and champions survive a restart.
- **Autoscaling on real backlog** — **KEDA** scales workers 1–10 on the Redis
  stream *lag* (not incidental CPU, which is meaningless once work is queued).
  Scale-down is safe: a terminated worker drains its in-flight batch and leaves
  unacked work on the stream to be reclaimed.
- **Observability** — every worker exposes Prometheus `/metrics` (steps/sec,
  halts, batch-duration histogram, busy gauge), scraped via a **ServiceMonitor**;
  the run-once coordinator pushes its summary to a **Pushgateway**. A Grafana
  dashboard ships as a ConfigMap. Stack: **kube-prometheus-stack**.
- **Minimal, hardened image** — two-stage Docker build producing a static
  `CGO_ENABLED=0` binary on `scratch` (~16 MB, no OS attack surface). One
  `ARG`-parameterised Dockerfile builds both the worker and coordinator.
- **Security context** — non-root, read-only root filesystem, no privilege
  escalation, all Linux capabilities dropped, `seccompProfile: RuntimeDefault`
  — across workers, Redis, and the Pushgateway.
- **Health probing** — liveness and readiness probes tuned so a failing pod is
  pulled from service rotation faster than it is killed.
- **Graceful lifecycle** — the server drains in-flight HTTP requests *and*
  finishes its in-flight batch on SIGTERM; the Deployment's grace period matches.
- **Zero-downtime rollouts** — rolling updates with max-1-unavailable / max-1-surge.
- **One-command deploy** — Kustomize root: `kubectl apply -k deploy/`.

## Tech stack

Go 1.24 · Docker (multi-stage, scratch) · Kubernetes (Deployment, StatefulSet,
Service, Job, Namespace) · Kustomize · Redis Streams · KEDA (queue-lag
autoscaling) · Prometheus + Grafana (kube-prometheus-stack, ServiceMonitor,
Pushgateway) · `log/slog` structured logging.

## Quick start

### Locally

```bash
# Run a built-in Busy Beaver program
go run ./cmd/emulator -program=bb4
go run ./cmd/emulator -program=bb2 -verbose   # step-by-step trace

# Start a worker (HTTP only)
go run ./cmd/server -addr :8080

# Drive a search via direct HTTP fan-out...
go run ./cmd/coordinator -workers=http://localhost:8080 -states=2

# ...or via the queue (needs a Redis; `docker run -p 6379:6379 redis`)
go run ./cmd/server -redis localhost:6379 &
go run ./cmd/coordinator -redis localhost:6379 -states=2
```

### On Kubernetes

```bash
# Build the images (one ARG-parameterised Dockerfile builds both)
docker build -t worker:latest .
docker build --build-arg CMD=coordinator -t coordinator:latest .

# Core stack: workers, Service, Redis (queue + state)
kubectl apply -k deploy/
kubectl get pods -n turing-cluster -w

# Autoscaling (KEDA) and observability (Prometheus/Grafana) are cluster add-ons
# installed via Helm, then wired in — see deploy/monitoring/README.md
helm upgrade --install keda kedacore/keda -n keda --create-namespace --wait
kubectl apply -f deploy/keda-scaledobject.yaml
kubectl apply -k deploy/monitoring/

# Run a search as an in-cluster Job
kubectl apply -f deploy/coordinator-job.yaml
kubectl -n turing-cluster logs -f job/turing-coordinator
```

## Results

Verified end-to-end **fully in-cluster** (coordinator Job + worker fleet + Redis):
all 20,736 n=2 machines evaluated, yielding **S(2) = 6** and **σ(2) = 4** — the
known Busy Beaver values. KEDA scaling was exercised on a live backlog: workers
scaled **1 → 5 → 10** on queue lag and back to 1 once drained.

## Status & roadmap

Done: emulator, HTTP worker, hardened image, Kubernetes manifests, brute-force
coordinator, a Redis Streams **work queue** (decoupled producer/consumers with
crash reclaim), **KEDA** autoscaling on queue lag, and a **Prometheus/Grafana**
observability stack. Planned next: symmetry-reduced enumeration (to make n≥3
feasible), CI/CD, and persisting champion records to a database. See
**[REPORT.md](REPORT.md)** for the full engineering write-up and
**[docs/DESIGN-queue-observability.md](docs/DESIGN-queue-observability.md)** for
the queue/observability design.

## License

See [LICENSE](LICENSE).
