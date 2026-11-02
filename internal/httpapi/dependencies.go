package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/Bpoe/dsc-registry/internal/diagnostics"
)

type requestStartKey struct{}

func (a *adminAPI) logDependencyFailure(r *http.Request, dependency, operation string, err error) {
	details := diagnostics.Details(err)
	// Client disconnects are not dependency outages. A request deadline remains
	// diagnostic; joined filesystem failures are retained even during cancellation.
	canceled := len(details) > 0
	for _, detail := range details {
		canceled = canceled && detail["kind"] == "canceled"
	}
	if r.Context().Err() == context.Canceled && (canceled || len(details) == 1 && errors.Is(err, sql.ErrTxDone)) {
		return
	}
	if dependency == "database" {
		for _, detail := range details {
			if detail["stage"] == "begin" || detail["stage"] == "commit" {
				operation = "transaction"
				break
			}
		}
	}
	attrs := []any{"dependency", dependency, "operation", operation, "error", details,
		"method", methodLabel(r.Method), "route", routeLabel(r.Pattern)}
	if start, ok := r.Context().Value(requestStartKey{}).(time.Time); ok {
		attrs = append(attrs, "duration", time.Since(start))
	}
	a.logger.ErrorContext(r.Context(), "dependency operation failed", attrs...)
}
