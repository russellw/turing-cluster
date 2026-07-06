# Design: Queue-Decoupled Search + Observability

Status: **proposed** (not yet implemented). Author's intent captured 2026-07-06.

This document is the reference design for the "queue + metrics" arc discussed in
[REPORT.md](../REPORT.md) Next Steps #1, #2, #3, and #5. It exists so the work
can be implemented in **independent phases across different sessions** — each
phase below is separately buildable and verifiable.

The point of this arc is to demonstrate Kubernetes competence the current
deployment does *not* yet show: **asynchronous/decoupled architecture, a
stateful workload, queue-driven autoscaling, and an observability stack.** It is
deliberately unconcerned with advancing the Busy Beaver frontier (no symmetry
reduction, no n≥3). See CLAUDE.md for why.

---

## 1. The architectural change

**Today:** an out-of-cluster `coordinator` process enumerates every candidate
machine and fans them out over `POST /run` to a fleet of stateless workers. The
"cluster" is really just a load-balanced service driven by an external client,
and the HPA scales on CPU that the external client happens to generate.

**Target:** a queue sits between a producer and the workers.

```
                         ┌─────────────────────────────┐
                         │        Redis (StatefulSet)   │
   coordinator           │  stream "jobs"  (work)       │        worker(s)
   (producer +   ──XADD──▶  consumer group "workers"    ◀──XREADGROUP──  consume
    result sink) ◀─XREAD──  stream "results" (champions)│     XACK        batch,
                         │  hash  "champions" (state)   │◀──HSET──  run locally,
                         └─────────────────────────────┘           emit champions
        │                                                                │
        └────────── /metrics ──────▶ Prometheus ──▶ Grafana ◀────── /metrics
                                          │
                                          └──▶ KEDA ScaledObject scales workers
                                               on "jobs" consumer-group lag
```

Key properties this buys us, each mapping to a K8s skill:

| Property | Kubernetes skill demonstrated |
|----------|-------------------------------|
| Producer and consumers decoupled by a durable stream | async architecture, no direct coupling |
| Redis holds queue + champion state on a PVC | StatefulSet / PVC / persistent workload |
| Workers scale on queue backlog, not incidental CPU | custom/external-metric autoscaling (KEDA) |
| `/metrics` → Prometheus → Grafana | observability stack, ServiceMonitor, dashboards |
| Champion state survives restart | resumable / durable search |

---

## 2. Technology choices (with rationale)

- **Queue: Redis Streams.** One dependency that covers three needs at once —
  work queue (consumer groups give at-least-once delivery, per-message `XACK`,
  and `XCLAIM` recovery of messages from dead consumers), champion state (a
  hash), and a natural scaling signal (consumer-group lag). Runs trivially
  in-cluster. *Rejected:* NATS JetStream (more moving parts for no gain here);
  native K8s Jobs per candidate (millions of tiny Jobs — absurd overhead; the
  batch design in §4 is the answer instead).

- **Autoscaling: KEDA `ScaledObject` on Redis Stream lag.** Purpose-built for
  "scale a Deployment on queue depth," including scale-to-zero. Much less
  plumbing than the alternative. *Alternative:* Prometheus Adapter feeding a
  custom-metrics HPA — keep as fallback if we'd rather not add the KEDA operator.

- **Metrics: `prometheus/client_golang`**, `/metrics` on worker and coordinator.

- **Prometheus + Grafana: `kube-prometheus-stack`** (Helm) providing the
  Prometheus Operator, so we wire scraping with `ServiceMonitor` CRDs and ship a
  Grafana dashboard as a ConfigMap. *Alternative:* hand-rolled Prometheus +
  Grafana Deployments if we want to avoid Helm and keep everything in Kustomize.

**Decisions (resolved in Phase 4):** kube-prometheus-stack (Helm) — done;
coordinator stays a run-once `Job`, with its metrics pushed to a **Pushgateway**
rather than scraped. **Still open (Phase 5):** KEDA vs Prometheus Adapter for the
queue-depth autoscaler. Defaults above.

---

## 3. Components and code changes

### 3.1 `cmd/coordinator` → producer + result sink

- Enumerate the transition space as today, but **enqueue batches** (§4), not
  individual machines: `XADD jobs * ...` one message per batch.
- Run a concurrent **result collector**: `XREAD` from `results`, fold each
  worker's champion candidate into running high-water marks, and mirror them to
  the `champions` hash (so state survives a coordinator restart).
- Terminate when all enqueued batches are acked (track expected vs. acked
  count, or drain until the `jobs` group's pending+backlog hits zero), then
  print S(n)/σ(n) and the winning tables — same final output as today.
- New config (env, K8s-friendly): `REDIS_ADDR`, `BATCH_SIZE`, plus existing
  `-states` / `-max-steps`. The `-workers` HTTP flag is retired in queue mode.

### 3.2 `cmd/server` (worker) → also a queue consumer

- **Keep** `POST /run` and `GET /healthz` unchanged (still useful for smoke
  tests and back-compat; `main_test.go` keeps passing).
- Add a **consumer loop** when `REDIS_ADDR` is set: `XREADGROUP` from `jobs`
  as consumer group `workers` (consumer name = pod hostname), expand each batch
  locally via `pkg/turing` **in-process** (no HTTP self-call), triage each
  machine, `XADD` champion candidates to `results`, then `XACK`.
- Recover orphaned work: periodic `XAUTOCLAIM` of messages idle past a
  threshold (a pod that died mid-batch).
- Add `GET /metrics` (§5).
- Consumer loop must honor the existing SIGTERM graceful-shutdown path: stop
  reading new batches, finish the in-flight batch, then exit within the drain
  window.

### 3.3 New package `pkg/search` (batching + triage)

Pure Go, no infra, fully unit-testable — this is the safe first code phase.

- `Batch{Start, Count uint64}` — a contiguous run of odometer indices into the
  transition space for a given `states`.
- `Enumerate(states, batchSize) iter.Seq[Batch]` — moved/derived from the
  coordinator's current odometer.
- `Expand(states, Batch) iter.Seq[*turing.Program]` — indices → transition
  tables (the existing mixed-radix odometer logic, factored out).
- `Champion{Steps, Sigma int64; Program ...}` and a `Fold` that combines two
  champions by max — used by both worker (local) and coordinator (global).

### 3.4 Redis (new stateful component)

- `StatefulSet` (1 replica) + headless `Service` + `PVC`.
- `appendonly yes` (AOF) via a ConfigMap-mounted `redis.conf`, so champion
  state and un-acked work survive a Redis pod restart.

---

## 4. Batch work protocol (why batches, not machines)

Individual candidates are tiny and numerous (n=2 is 20,736; n=3 is ~16.7M).
One queue message per machine would make the queue the bottleneck we're trying
to remove. Instead the producer enqueues **index ranges** into the mixed-radix
transition space, and each worker expands its range locally.

- `jobs` message = `{states, start, count}` (a `Batch`). `BATCH_SIZE` default
  ~1000 machines; tune so a batch is ~100ms–1s of work.
- `results` message = a champion candidate `{steps, sigma, program}` — workers
  emit only local champions per batch, not every machine, keeping `results`
  small.

**Idempotency / at-least-once:** champions combine by *max*, so reprocessing a
redelivered batch can never corrupt the result — it can only recompute the same
maxima. This makes at-least-once delivery safe with zero dedup logic. Call this
out; it's the property that makes the whole queue design clean.

---

## 5. Metrics

Exposed on `/metrics` (Prometheus text format) by worker and coordinator.

**Worker:**
- `turing_candidates_total` (counter) — machines evaluated
- `turing_halts_total` (counter)
- `turing_steps_total` (counter) — total steps executed (drives steps/sec panel)
- `turing_batch_duration_seconds` (histogram)
- `turing_worker_busy` (gauge 0/1)

**Coordinator:**
- `turing_batches_enqueued_total` (counter)
- `turing_batches_acked_total` (counter)
- `turing_jobs_pending` (gauge) — sampled `XLEN`/group lag; also the scale signal
- `turing_champion_steps`, `turing_champion_sigma` (gauges)

**Scaling wiring:** KEDA `ScaledObject` targets the `jobs` consumer-group
**lag** (KEDA's `redis-streams` scaler reads this directly from Redis — it does
*not* need Prometheus). Grafana reads the same backlog for its queue-depth panel
from `turing_jobs_pending`. The existing CPU HPA is removed or demoted to a
secondary trigger, because CPU is no longer the honest signal.

---

## 6. Manifests (`deploy/`)

New / changed, all wired through `kustomization.yaml`:

| File | Resource | Notes |
|------|----------|-------|
| `redis/statefulset.yaml` | StatefulSet + PVC | AOF on, ConfigMap `redis.conf` |
| `redis/service.yaml` | headless Service | stable DNS for `REDIS_ADDR` |
| `redis/configmap.yaml` | ConfigMap | `appendonly yes` |
| `coordinator-job.yaml` | Job (or Deployment) | producer + result sink; `REDIS_ADDR` env |
| `worker-deployment.yaml` | **edit** | add `REDIS_ADDR`, `metrics` port; security context unchanged |
| `keda-scaledobject.yaml` | ScaledObject | `redis-streams` trigger on `jobs` lag |
| `worker-hpa.yaml` | **remove or demote** | superseded by KEDA |
| `monitoring/servicemonitor-worker.yaml` | ServiceMonitor | scrape worker `/metrics` |
| `monitoring/servicemonitor-coordinator.yaml` | ServiceMonitor | scrape coordinator `/metrics` |
| `monitoring/grafana-dashboard.yaml` | ConfigMap | dashboard JSON (auto-loaded by Grafana sidecar) |

kube-prometheus-stack and KEDA are installed as cluster add-ons (Helm),
documented in README, not vendored into `deploy/`.

---

## 7. Phased implementation plan

Each phase is independently buildable and verifiable — safe to split across
sessions. Ordered so infra can be validated before app code depends on it.

- **Phase 0 — Redis in-cluster. ✅ done 2026-07-06.** Added `deploy/redis/*`
  (ConfigMap with AOF config, headless Service, StatefulSet + 1Gi PVC) and wired
  into `kustomization.yaml`. Verified on a live cluster: pod Ready, `redis-cli
  ping` → PONG, `appendonly yes`, an `XADD`/`XLEN` stream round-trip, the stable
  DNS name `turing-redis.turing-cluster.svc.cluster.local` resolves, and a
  written key survived a pod delete/reschedule onto the same PVC.

- **Phase 1 — `pkg/search`. ✅ done 2026-07-06.** Created `pkg/search` as the
  canonical enumeration: `Batch`, `Alphabet`/`StateNames`, `SpaceSize`,
  `Enumerate` (space → batches), `ProgramAt`/`Programs`/`Expand` (index →
  machine), and `Tally` (fold outcomes by max — the shared champion accumulator
  for both worker and coordinator). `cmd/coordinator` was refactored to use it
  (its odometer/alphabet/champion code deleted; `SearchResult = search.Tally`).
  Verified: full n=2 enumeration is 20,736 distinct valid machines and an
  in-process run reproduces S(2)=6, σ(2)=4; batches cover the space exactly and
  expand to the same sequence as the flat enumeration; `go build/vet/test ./...`
  all green.

- **Phase 2 — queue path. ✅ done 2026-07-06.** Added `pkg/queue` (go-redis
  wrapper: streams `jobs`/`results`, group `workers`, `champions` hash; batch
  enqueue, `XREADGROUP`+`XAUTOCLAIM` consume, ack, outcome publish/read, champion
  mirror). `cmd/server` gained a consumer loop gated on `REDIS_ADDR` (unset =
  HTTP-only, no regression) that finishes its in-flight batch on SIGTERM.
  `cmd/coordinator` gained a `-redis` producer + result-sink mode alongside the
  HTTP one. Dockerfile parameterised by `ARG CMD` to build both binaries;
  `deploy/worker-deployment.yaml` sets `REDIS_ADDR`; added
  `deploy/coordinator-job.yaml`. Go bumped 1.22→1.24 by the redis dep.
  Verified twice: (a) worker+coordinator binaries against the in-cluster Redis
  via port-forward, and (b) **fully in-cluster** — coordinator Job + 3 worker
  pods + Redis StatefulSet — both yielding S(2)=6, σ(2)=4 over 20,736 machines
  (9,784 halting), with the `champions` hash mirrored and 0 pending jobs.
  Not yet exercised: the `XAUTOCLAIM` reclaim path (happy-path run has no crashes)
  and cross-run isolation beyond the results-cursor guard (a run-id tag is the
  refinement). `coordinator-job.yaml` is applied on demand, not in the kustomize
  root (a run-once Job doesn't belong in the base).

- **Phase 3 — `/metrics`. ✅ done 2026-07-06.** Added `client_golang`. Worker
  exposes `GET /metrics` on the existing HTTP port (8080) with the §5 counters +
  `turing_batch_duration_seconds` histogram + `turing_worker_busy` gauge,
  instrumented in `processJob`. Coordinator serves `/metrics` on `:2112`
  (`-metrics-addr`, queue mode only) with enqueued/acked/`jobs_pending` and the
  champion gauges, instrumented in `RunQueueSearch`. No new worker port was
  needed — `/metrics` rides the existing port, which simplifies the Phase 4
  ServiceMonitor. Verified in-cluster: a worker's counters advanced over a search
  (batches/candidates/halts/steps, histogram count=16, busy toggled); and with
  workers scaled to 0 the coordinator showed `batches_enqueued=jobs_pending=208`,
  then champion gauges climbed to S=6/σ=4 as the fleet drained. **Caveat:** the
  coordinator is a run-once Job, so its `/metrics` is only live during a run —
  reliably capturing it needs a Pushgateway (or a long-lived coordinator), which
  Phase 4 must decide. The always-on, dashboard-driving signal is the worker's.

- **Phase 4 — Prometheus + Grafana. ✅ done 2026-07-06.** Installed
  kube-prometheus-stack (lean: Prometheus + Grafana + operator; values in
  `deploy/monitoring/kube-prometheus-stack-values.yaml`, scrapes all
  ServiceMonitors, Grafana dashboard sidecar `searchNamespace: ALL`). Added
  `deploy/monitoring/`: worker ServiceMonitor, a **Pushgateway** (Deployment +
  Service + ServiceMonitor) that resolves the deferred coordinator-metrics
  question, and the dashboard ConfigMap. The coordinator now pushes its final
  summary to the gateway (`-pushgateway` / `PUSHGATEWAY_ADDR`, `push` package —
  no new dep). Verified: all targets UP (3 worker pods + pushgateway); Prometheus
  answers `turing_champion_steps=6`, `turing_champion_sigma=4`,
  `turing_batches_enqueued_total=42`, `count(turing_worker_busy)=3`; Grafana
  imported the dashboard at `/d/turing-cluster/` (9 panels) with a healthy
  Prometheus datasource. Monitoring manifests are kept out of the root kustomize
  (they need the operator CRDs first) — applied via `kubectl apply -k
  deploy/monitoring/` after the Helm install; see `deploy/monitoring/README.md`.
  **Decision recorded:** worker metrics are scraped (always-on, drive the
  dashboard); the run-once coordinator uses the Pushgateway batch-job pattern.

- **Phase 5 — KEDA.** Install KEDA; add the ScaledObject; remove/demote the CPU
  HPA. Verify: enqueue a large-n batch load and watch replicas scale up on lag
  and back down as the backlog drains.

- **Phase 6 — docs.** Update REPORT.md (move items 1–3, 5 from Next Steps to
  Built), README.md architecture section + add the Grafana screenshot, and flip
  this doc's status to *implemented*.

---

## 8. What this explicitly does NOT do

- No symmetry reduction / normal-form enumeration, no n≥3. (Not about Turing
  machines — see CLAUDE.md.) The batch design *would* let n≥3 run, but that's a
  separate concern.
- No re-queuing of non-halted candidates for more steps. Possible later (emit a
  continuation `Batch` with the machine's snapshot) but out of scope here.
