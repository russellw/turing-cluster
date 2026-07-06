package main

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// serveMetrics starts a background HTTP server exposing GET /metrics. It runs for
// the life of the process; a run-once Job's server dies with the Job.
func serveMetrics(addr string) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		slog.Info("serving metrics", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server error", "err", err)
		}
	}()
}

// Coordinator metrics. NOTE: the coordinator runs as a run-once Job, so this
// endpoint is only live for the duration of a search. Reliably capturing these
// needs a Pushgateway (or a long-lived coordinator) — a Phase 4 decision. See
// docs/DESIGN-queue-observability.md §5.
var (
	batchesEnqueued = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_batches_enqueued_total",
		Help: "Total batches enqueued onto the jobs stream this run.",
	})
	batchesAcked = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_batches_acked_total",
		Help: "Total batch outcomes received back from workers this run.",
	})
	jobsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "turing_jobs_pending",
		Help: "Batches enqueued but not yet reported back (backlog).",
	})
	championSteps = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "turing_champion_steps",
		Help: "Most steps before halting seen so far, S(n).",
	})
	championSigma = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "turing_champion_sigma",
		Help: "Most 1s left on the tape seen so far, sigma(n).",
	})
)

func init() {
	prometheus.MustRegister(
		batchesEnqueued,
		batchesAcked,
		jobsPending,
		championSteps,
		championSigma,
	)
}
