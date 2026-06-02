// Package observability wires structured logging and operational endpoints.
package observability

import (
	"expvar"
	"log/slog"
	"net/http"
	"os"
)

// NewLogger returns a JSON slog.Logger at the given level
// (debug|info|warn|error; defaults to info).
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: redactSensitive,
	}))
}

// RegisterHealth mounts liveness, readiness, and metrics endpoints on mux.
// Readiness is a stub here; Phase 2 gates it on database connectivity.
func RegisterHealth(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", expvar.Handler())
}
