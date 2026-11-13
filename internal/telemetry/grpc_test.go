package telemetry

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracespb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type traceReceiver struct {
	tracespb.UnimplementedTraceServiceServer
	requests atomic.Int64
}

func (r *traceReceiver) Export(context.Context, *tracespb.ExportTraceServiceRequest) (*tracespb.ExportTraceServiceResponse, error) {
	r.requests.Add(1)
	return &tracespb.ExportTraceServiceResponse{}, nil
}

type metricReceiver struct {
	metricspb.UnimplementedMetricsServiceServer
	requests atomic.Int64
}

func (r *metricReceiver) Export(context.Context, *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	r.requests.Add(1)
	return &metricspb.ExportMetricsServiceResponse{}, nil
}

type logReceiver struct {
	logspb.UnimplementedLogsServiceServer
	requests atomic.Int64
}

func (r *logReceiver) Export(context.Context, *logspb.ExportLogsServiceRequest) (*logspb.ExportLogsServiceResponse, error) {
	r.requests.Add(1)
	return &logspb.ExportLogsServiceResponse{}, nil
}

func TestOTLPGRPC(t *testing.T) {
	cleanEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	defer server.Stop()
	traces, metrics, logs := &traceReceiver{}, &metricReceiver{}, &logReceiver{}
	logspb.RegisterLogsServiceServer(server, logs)
	tracespb.RegisterTraceServiceServer(server, traces)
	metricspb.RegisterMetricsServiceServer(server, metrics)
	go func() { _ = server.Serve(listener) }()
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	logger, shutdown, err := Init(context.Background(), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "grpc test")
	span.End()
	logger.Info("gRPC test")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if traces.requests.Load() < 1 || metrics.requests.Load() < 1 || logs.requests.Load() != 1 {
		t.Fatal("missing gRPC export")
	}
}
