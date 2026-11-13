package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Bpoe/dsc-registry/internal/service"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func New(app *service.Service, requestTimeout, pollMaxAge time.Duration, maxRequestBytes, maxReportBytes int64, logger *slog.Logger) http.Handler {
	admin := &adminAPI{service: app, maxRequestBytes: maxRequestBytes, maxReportBytes: maxReportBytes,
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
	return otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(response, r)
		// otelhttp derives metric routes and span names from Request.Pattern.
		// Set the trace route after ServeMux matched and replace its raw URL path
		// so machine/report identifiers are not exported by default.
		span := trace.SpanFromContext(r.Context())
		route := routeLabel(r.Pattern)
		span.SetAttributes(attribute.String("url.path", route))
		if r.Pattern != "" {
			span.SetAttributes(attribute.String("http.route", route))
		}
		// This otelhttp release reports status codes but does not infer the
		// stable error.type for 5xx. Add it through the library's labeler rather
		// than creating another HTTP instrument.
		if response.status >= 500 && response.status < 600 {
			failure := attribute.String("error.type", strconv.Itoa(response.status))
			span.SetAttributes(failure)
			if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
				labeler.Add(failure)
			}
		}
		args := []any{"method", methodLabel(r.Method), "route", route,
			"status", response.status, "duration", time.Since(start)}
		if sc := span.SpanContext(); sc.IsValid() {
			args = append(args, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
		}
		logger.InfoContext(r.Context(), "http request", args...)
	}), "dscregd", otelhttp.WithSpanOptions(trace.WithAttributes(
		// Suppress raw paths from span creation, including panic/unwind paths.
		attribute.String("url.path", ""),
	)))
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
