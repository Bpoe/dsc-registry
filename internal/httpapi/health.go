package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
)

func New(db *database.Store, reports *reportstore.Store, events eventing.Dispatcher, requestTimeout, pollMaxAge time.Duration, maxRequestBytes, maxReportBytes int64, logger *slog.Logger) http.Handler {
	if events == nil {
		events = eventing.Noop{}
	}

	admin := &adminAPI{store: db, reports: reports, events: events, maxRequestBytes: maxRequestBytes, maxReportBytes: maxReportBytes,
		timeout: requestTimeout, pollMaxAge: pollMaxAge, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	// New is called only after startup validation; net/http owns admission
	// and graceful shutdown. Shared dependencies do not determine readiness.
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	admin.registerRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(response, r)
		logger.Info("http request", "method", methodLabel(r.Method), "route", routeLabel(r.Pattern),
			"status", response.status, "duration", time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
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
	status      int
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status >= 200 {
		w.wroteHeader = true
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
