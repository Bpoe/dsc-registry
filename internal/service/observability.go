package service

import (
	"context"
	"time"

	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Reused for ingestion and publication; sub-millisecond calls and multi-second
// remote filesystem stalls remain distinguishable.
var durationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

type reportMetrics struct {
	ingest  metric.Float64Histogram
	size    metric.Int64Histogram
	publish metric.Float64Histogram
}

func newReportMetrics() reportMetrics {
	meter := otel.Meter("github.com/Bpoe/dsc-registry/internal/service")
	ingest, err := meter.Float64Histogram("dscreg.report.ingest.duration", metric.WithUnit("s"), metric.WithDescription("Complete service report ingestion attempts"), metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		otel.Handle(err)
	}
	size, err := meter.Int64Histogram("dscreg.report.size", metric.WithUnit("By"), metric.WithDescription("Original parsed DSC result envelope size"), metric.WithExplicitBucketBoundaries(1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824))
	if err != nil {
		otel.Handle(err)
	}
	publish, err := meter.Float64Histogram("dscreg.report.publish.duration", metric.WithUnit("s"), metric.WithDescription("Cumulative database publication time per service attempt, excluding filesystem I/O"), metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		otel.Handle(err)
	}
	return reportMetrics{ingest: ingest, size: size, publish: publish}
}

// databasePhase measures only service-orchestrated database work. Each retry
// contributes to one cumulative publication measurement, without including the
// intervening filesystem read/write or cleanup.
func (s *Service) databasePhase(ctx context.Context, name string) (context.Context, trace.Span, time.Time) {
	ctx, span := s.tracer.Start(ctx, name)
	return ctx, span, time.Now()
}
func finishDatabasePhase(span trace.Span, start time.Time, total *time.Duration, err error) {
	*total += time.Since(start)
	telemetry.RecordError(span, err)
	span.End()
}
