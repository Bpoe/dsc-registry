package database

import (
	"context"
	"database/sql/driver"
	"errors"

	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"
)

func sqlTelemetryOptions(system string) []otelsql.Option {
	return []otelsql.Option{
		otelsql.WithAttributes(attribute.String("db.system.name", system)),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableQuery:         true,
			DisableErrSkip:       true,
			OmitRows:             true,
			OmitConnResetSession: true,
			// Driver errors can contain parameter values or DSNs. Keep safe error
			// attributes, but suppress the library's raw exception message events.
			RecordError: func(error) bool { return false },
		}),
		otelsql.WithDisableSkipErrMeasurement(true),
		otelsql.WithAttributesGetter(func(_ context.Context, method otelsql.Method, _ string, _ []driver.NamedValue) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("db.operation.name", string(method))}
		}),
		otelsql.WithSpanErrorAttributesGetter(func(err error) []attribute.KeyValue {
			if errors.Is(err, driver.ErrSkip) {
				return nil
			}
			return []attribute.KeyValue{attribute.String("error.type", telemetry.ErrorType(err))}
		}),
	}
}

// Close releases pool observers before the database. It is safe to repeat.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.poolMetrics != nil {
			s.closeErr = s.poolMetrics.Unregister()
		}
		s.closeErr = errors.Join(s.closeErr, s.DB.Close())
	})
	return s.closeErr
}
