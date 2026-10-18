package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/reportstore"
)

const unavailable = `{"error":{"code":"not_ready","message":"Service dependencies are unavailable"}}` + "\n"

func New(db *sql.DB, reports *reportstore.Store, timeout time.Duration, logger *slog.Logger) http.Handler {
	// A timed-out filesystem call cannot be interrupted portably. Allow only one
	// dependency check in flight, including after its HTTP response times out.
	probing := make(chan struct{}, 1)
	ready := http.TimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probing <- struct{}{}:
			defer func() { <-probing }()
		default:
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service dependencies are unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		dependency := "database"
		err := db.PingContext(ctx)
		if err == nil {
			dependency = "report_storage"
			err = reports.Check(ctx)
		}
		if err != nil {
			logger.Warn("readiness check failed", "dependency", dependency)
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service dependencies are unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}), timeout, unavailable)

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
			writeError(w, http.StatusNotFound, "not_found", "Endpoint not found")
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	// These responses contain only fixed strings, and cannot fail to marshal.
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
