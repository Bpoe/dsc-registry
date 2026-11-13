package reportstore

import (
	"context"
	"time"

	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func newStorageMetric() metric.Float64Histogram {
	histogram, err := otel.Meter("github.com/Bpoe/dsc-registry/internal/reportstore").Float64Histogram(
		"dscreg.report.storage.operation.duration", metric.WithUnit("s"),
		metric.WithDescription("Wall time of attempted confined report filesystem operations"),
		metric.WithExplicitBucketBoundaries(.0001, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60))
	if err != nil {
		otel.Handle(err)
	}
	return histogram
}

func (s *Store) recordOperation(ctx context.Context, operation string, start time.Time, err error) {
	attrs := []attribute.KeyValue{attribute.String("dscreg.storage.operation", operation)}
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", telemetry.ErrorType(err)))
	}
	s.operationDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}
