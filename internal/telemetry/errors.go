package telemetry

import (
	"context"
	"errors"
	"io"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrorType is deliberately bounded and never includes paths or error messages.
func ErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrExist):
		return "already_exists"
	case errors.Is(err, io.ErrShortWrite):
		return "short_write"
	default:
		return "other"
	}
}

// RecordError excludes wrapped diagnostics which can contain credentials,
// paths, or report contents. Callers retain the original error for handling.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	kind := ErrorType(err)
	span.SetAttributes(attribute.String("error.type", kind))
	span.SetStatus(codes.Error, kind)
	span.RecordError(errors.New(kind))
}
