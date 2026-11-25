package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

func recordMachine(ctx context.Context, id, name string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(telemetry.MachineID.String(id), telemetry.MachineName.String(name))
	}
}

func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodPatch, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

func routeLabel(pattern string) string {
	if _, route, ok := strings.Cut(pattern, " "); ok {
		return route
	}
	return "unmatched"
}
