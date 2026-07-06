package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/russellwallace/turing-cluster/pkg/queue"
	"github.com/russellwallace/turing-cluster/pkg/turing"
)

func main() {
	addr := flag.String("addr", envOr("PORT", "8080"), "listen address (port or host:port)")
	redisAddr := flag.String("redis", os.Getenv("REDIS_ADDR"), "redis host:port; if set, also consume batch jobs from the queue")
	stepLimit := flag.Int64("step-limit", envInt64("STEP_LIMIT", 1000), "per-candidate step limit for queued batches")
	flag.Parse()

	// Normalize: bare port number → ":port"
	if len(*addr) > 0 && (*addr)[0] != ':' {
		*addr = ":" + *addr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /run", handleRun)
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:         *addr,
		Handler:      withLogging(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Minute, // long-running machines need time
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Optionally consume batch jobs from the queue (Phase 2). When -redis/
	// REDIS_ADDR is unset the worker is HTTP-only, exactly as before.
	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	var consumerDone chan struct{}
	if *redisAddr != "" {
		qc := queue.Dial(*redisAddr)
		defer qc.Close()
		consumerDone = make(chan struct{})
		go func() {
			defer close(consumerDone)
			if err := runConsumer(consumerCtx, qc, *stepLimit); err != nil {
				slog.Error("queue consumer exited", "err", err)
			}
		}()
		slog.Info("queue consumer enabled", "redis", *redisAddr)
	}

	// Graceful shutdown on SIGTERM / SIGINT (Kubernetes sends SIGTERM).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	slog.Info("shutting down")
	// Stop taking new batches; the in-flight batch finishes on its own context.
	stopConsumer()
	if consumerDone != nil {
		select {
		case <-consumerDone:
		case <-time.After(30 * time.Second):
			slog.Warn("consumer did not stop within drain window")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}

// RunRequest is the body for POST /run.
type RunRequest struct {
	Program  *turing.Program `json:"program"`
	MaxSteps int64           `json:"max_steps"` // 0 = unlimited
}

// RunResponse is the response for POST /run.
type RunResponse struct {
	Snapshot  *turing.Snapshot `json:"snapshot"`
	Halted    bool             `json:"halted"`
	ElapsedMS int64            `json:"elapsed_ms"`
	Error     string           `json:"error,omitempty"`
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Program == nil {
		writeError(w, http.StatusBadRequest, "missing program")
		return
	}

	m, err := turing.New(req.Program, req.MaxSteps)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid program: "+err.Error())
		return
	}

	start := time.Now()
	runErr := m.Run()
	elapsed := time.Since(start)

	resp := RunResponse{
		Snapshot:  m.Snapshot(),
		Halted:    m.Halted(),
		ElapsedMS: elapsed.Milliseconds(),
	}
	if runErr != nil {
		resp.Error = runErr.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(RunResponse{Error: msg})
}

// withLogging wraps a handler to log each request.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
