package main

import "github.com/prometheus/client_golang/prometheus"

// Worker metrics, exposed on GET /metrics (same port as the rest of the API).
// These are the continuously-scraped signals that drive the throughput/steps
// dashboard and, indirectly, capacity decisions. See
// docs/DESIGN-queue-observability.md §5.
var (
	candidatesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_candidates_total",
		Help: "Total candidate machines evaluated.",
	})
	haltsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_halts_total",
		Help: "Total candidate machines that halted within the step limit.",
	})
	stepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_steps_total",
		Help: "Total Turing-machine steps executed (rate() gives steps/sec).",
	})
	batchesProcessedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "turing_batches_processed_total",
		Help: "Total batches consumed and acked.",
	})
	batchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "turing_batch_duration_seconds",
		Help:    "Wall-clock time to evaluate one batch.",
		Buckets: prometheus.DefBuckets,
	})
	workerBusy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "turing_worker_busy",
		Help: "1 while the worker is evaluating a batch, 0 while idle.",
	})
)

func init() {
	prometheus.MustRegister(
		candidatesTotal,
		haltsTotal,
		stepsTotal,
		batchesProcessedTotal,
		batchDuration,
		workerBusy,
	)
}
