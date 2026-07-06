# turing-cluster

Distributed Turing machine evaluation on Kubernetes — with production-grade
operational characteristics.

A Turing machine emulator, an HTTP worker that runs as a Kubernetes pod, and a
coordinator that fans work out across a fleet of workers to search for
[Busy Beaver](https://en.wikipedia.org/wiki/Busy_beaver) numbers. The emulator
is the excuse; the point is the operational engineering around it —
containerisation, autoscaling, health probing, graceful lifecycle handling, and
zero-downtime deploys.

## Architecture

```
                 ┌──────────────┐        POST /run         ┌──────────────┐
                 │              │ ───────────────────────► │  worker pod  │
   candidate     │ coordinator  │                          ├──────────────┤
   machines ───► │  (search     │ ───────────────────────► │  worker pod  │
                 │   driver)    │                          ├──────────────┤
                 │              │ ───────────────────────► │  worker pod  │
                 └──────────────┘   round-robin fan-out    └──────────────┘
                                                            (HPA: 2–10 replicas)
```

- **`pkg/turing`** — a correct, fast 2-symbol Turing machine emulator. Sparse
  infinite tape, O(1) transitions, and full state **snapshot/restore** so a
  machine's execution can be checkpointed and handed between workers.
- **`cmd/server`** — wraps the emulator in an HTTP server (`POST /run`,
  `GET /healthz`) that runs as a Kubernetes pod.
- **`cmd/coordinator`** — enumerates candidate machines and distributes them
  across the worker fleet, reporting the Busy Beaver champions.
- **`cmd/emulator`** — a CLI for running built-in programs locally.

## The Kubernetes & operations story

This is where the project earns its keep:

- **Minimal, hardened image** — two-stage Docker build producing a static
  `CGO_ENABLED=0` binary on `scratch` (~7 MB, no OS attack surface).
- **Security context** — runs as non-root (`nobody`), read-only root filesystem,
  no privilege escalation, all Linux capabilities dropped, `seccompProfile:
  RuntimeDefault`.
- **Autoscaling** — a HorizontalPodAutoscaler scales workers 2–10 on CPU, with
  deliberately conservative scale-down to protect long-running machines mid-execution.
- **Health probing** — liveness and readiness probes on `/healthz`, tuned so a
  failing pod is pulled from service rotation faster than it is killed.
- **Graceful lifecycle** — the server drains in-flight requests on SIGTERM
  within a 30s window; the Deployment's grace period is sized to match.
- **Zero-downtime rollouts** — rolling updates with max-1-unavailable / max-1-surge.
- **One-command deploy** — Kustomize root: `kubectl apply -k deploy/`.

## Tech stack

Go 1.22 · Docker (multi-stage, scratch) · Kubernetes (Deployment, Service,
HPA, Namespace) · Kustomize · `log/slog` structured logging.

## Quick start

### Locally

```bash
# Run a built-in Busy Beaver program
go run ./cmd/emulator -program=bb4
go run ./cmd/emulator -program=bb2 -verbose   # step-by-step trace

# Start a worker
go run ./cmd/server -addr :8080

# Drive a Busy Beaver search across it
go run ./cmd/coordinator -workers=http://localhost:8080 -states=2
```

### On Kubernetes

```bash
# Build and push the worker image
docker build -t ghcr.io/russellwallace/turing-cluster/worker:latest .
docker push ghcr.io/russellwallace/turing-cluster/worker:latest

# Deploy the whole stack
kubectl apply -k deploy/
kubectl get pods -n turing-cluster -w

# Run the coordinator against the in-cluster Service
go run ./cmd/coordinator -workers=http://turing-worker -states=2
```

## Results

The coordinator has been verified end-to-end against a live worker: all 20,736
n=2 machines evaluated, yielding **S(2) = 6** and **σ(2) = 4** — the known Busy
Beaver values.

## Status & roadmap

Working today: emulator, HTTP worker, Docker image, Kubernetes manifests, and a
first-cut brute-force coordinator. Planned next: symmetry-reduced enumeration
(to make n≥3 feasible), a durable work queue, persistent storage, CI/CD, and
Prometheus/Grafana observability. See **[REPORT.md](REPORT.md)** for the full
engineering write-up.

## License

See [LICENSE](LICENSE).
