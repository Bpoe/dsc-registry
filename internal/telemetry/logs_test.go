package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestLogsOnlyAndExplicitDisable(t *testing.T) {
	cleanEnvironment(t)
	received := make(chan *logspb.ExportLogsServiceRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/logs" || r.Header.Get("Authorization") != "Bearer logs-token" {
			t.Error("log-specific endpoint or header was ignored")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := &logspb.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(data, request); err != nil {
			t.Error(err)
			return
		}
		select {
		case received <- request:
		default:
			t.Error("unexpected additional logs export")
		}
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/custom/logs")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20common-token")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "authorization=Bearer%20logs-token")

	tp, mp, defaultLogger := otel.GetTracerProvider(), otel.GetMeterProvider(), slog.Default()
	local := slog.New(slog.NewJSONHandler(io.Discard, nil))
	logger, shutdown, err := Init(context.Background(), "dev", local)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("logs only")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp || slog.Default() != defaultLogger {
		t.Fatal("logs-only initialization changed global providers or logger")
	}
	select {
	case request := <-received:
		resources := request.GetResourceLogs()
		if len(resources) != 1 || len(resources[0].GetScopeLogs()) != 1 || len(resources[0].GetScopeLogs()[0].GetLogRecords()) != 1 {
			t.Fatalf("expected exactly one log record: %v", request)
		}
		record := resources[0].ScopeLogs[0].LogRecords[0]
		if record.GetBody().GetStringValue() != "logs only" || len(record.GetTraceId()) != 0 || len(record.GetSpanId()) != 0 {
			t.Fatal("unexpected log body or trace context", record)
		}
	default:
		t.Fatal("logs-only export missing")
	}

	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	logger, shutdown, err = Init(context.Background(), "dev", local)
	if err != nil {
		t.Fatal(err)
	}
	if logger != local {
		t.Fatal("explicitly disabling log export replaced the local logger")
	}
	logger.Info("local only")
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 {
		t.Fatal("disabled log exporter sent a request")
	}
}

func TestLogShutdownDeadline(t *testing.T) {
	cleanEnvironment(t)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/v1/logs")
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "1")
	local := slog.New(slog.NewJSONHandler(io.Discard, nil))
	logger, shutdown, err := Init(context.Background(), "dev", local)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("start export")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("log export did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("log shutdown ignored its deadline")
	}
}
