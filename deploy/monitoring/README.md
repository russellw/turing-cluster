# Monitoring (Phase 4)

Prometheus + Grafana observability for the search, via the Prometheus Operator.

## What's here

| File | Resource | Purpose |
|------|----------|---------|
| `kube-prometheus-stack-values.yaml` | Helm values | Lean stack: Prometheus + Grafana + operator only |
| `servicemonitor-worker.yaml` | ServiceMonitor | Scrapes each worker's `/metrics` (steps/sec, halts, busy, batch duration) |
| `pushgateway.yaml` | Deployment + Service | Pushgateway — holds the run-once coordinator Job's final metrics |
| `servicemonitor-pushgateway.yaml` | ServiceMonitor | Scrapes the Pushgateway (`honorLabels: true`) |
| `grafana-dashboard.yaml` | ConfigMap | The dashboard, auto-imported by the Grafana sidecar |

**Why a Pushgateway?** The coordinator runs as a run-once Job, so its own
`/metrics` endpoint dies with it. Pushing the final summary (champions, totals)
to a Pushgateway is the standard way to make a batch job's metrics outlive it and
land on the dashboard. Worker metrics are scraped directly — they're always-on.

## Install

The stack is a cluster add-on (Helm), not part of the root `deploy/`
kustomization — and these manifests depend on its CRDs, so they're applied
second:

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  -f deploy/monitoring/kube-prometheus-stack-values.yaml --wait

kubectl apply -k deploy/monitoring/
```

## Access

```bash
# Grafana (admin / admin) → dashboard "Turing Cluster — Busy Beaver Search"
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
# Prometheus
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090
```

Run a search (`kubectl apply -f deploy/coordinator-job.yaml`) and watch steps/sec,
busy workers, and batch-duration quantiles move; the last-run champions appear
once the Job pushes to the gateway.
