package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
)

const unavailable = `{"error":{"code":"not_ready","message":"Service dependencies are unavailable"}}` + "\n"

func New(db *database.Store, reports *reportstore.Store, timeout, requestTimeout, pollMaxAge time.Duration, maxRequestBytes, maxReportBytes int64, logger *slog.Logger) http.Handler {
	ready := &readinessProbe{timeout: timeout, check: func(ctx context.Context) error {
		dependency := "database"
		err := db.DB.PingContext(ctx)
		if err == nil {
			dependency = "report_storage"
			err = reports.Check(ctx)
		}
		if err != nil {
			logger.Warn("readiness check failed", "dependency", dependency)
		}
		return err
	}}

	admin := &adminAPI{store: db, reports: reports, maxRequestBytes: maxRequestBytes, maxReportBytes: maxReportBytes,
		timeout: requestTimeout, pollMaxAge: pollMaxAge, logger: logger}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/health/live", "/health/ready":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
				return
			}
			if r.URL.Path == "/health/live" {
				writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
			} else {
				ready.ServeHTTP(w, r)
			}
		default:
			admin.ServeHTTP(w, r)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		handler.ServeHTTP(response, r)
		route := "unmatched"
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			route = r.URL.Path
		}
		logger.Info("http request", "method", r.Method, "route", route,
			"status", response.status, "duration", time.Since(start))
	})
}

type probeResult struct {
	done chan struct{}
	err  error
}

type readinessProbe struct {
	timeout time.Duration
	check   func(context.Context) error
	mu      sync.Mutex
	active  *probeResult
}

func (p *readinessProbe) start() *probeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != nil {
		return p.active
	}
	result := &probeResult{done: make(chan struct{})}
	p.active = result
	// A caller's cancellation must not cancel other callers' shared check.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	go func() {
		defer cancel()
		err := p.check(ctx)
		if err == nil {
			err = ctx.Err()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		result.err = err
		close(result.done)
		p.active = nil
	}()
	return result
}

func (p *readinessProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	result := p.start()
	select {
	case <-result.done:
		if result.err == nil && ctx.Err() == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
	case <-ctx.Done():
	}
	// Keep the shared check active until it actually finishes: portable
	// filesystem calls may remain blocked after their context expires.
	writeError(w, http.StatusServiceUnavailable, "not_ready", "Service dependencies are unavailable")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	// Responses contain concrete JSON-safe types and cannot fail to marshal.
	// Transport failures are handled by net/http.
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{Error: apiError{Code: code, Message: message}})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
