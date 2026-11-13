package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracespb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_METRICS_PRODUCERS", "OTEL_GO_X_DEPRECATED_RUNTIME_METRICS"} {
		t.Setenv(key, "")
	}
	for _, signal := range []string{"", "_TRACES", "_METRICS"} {
		for _, option := range []string{"ENDPOINT", "PROTOCOL", "HEADERS", "CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
			t.Setenv("OTEL_EXPORTER_OTLP"+signal+"_"+option, "")
		}
	}
	tp, mp, prop, handler := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
	t.Cleanup(func() {
		if otel.GetTracerProvider() != tp {
			otel.SetTracerProvider(tp)
		}
		if otel.GetMeterProvider() != mp {
			otel.SetMeterProvider(mp)
		}
		if otel.GetTextMapPropagator() != prop {
			otel.SetTextMapPropagator(prop)
		}
		otel.SetErrorHandler(handler)
	})
}

func TestNoDestinationDoesNotInitializeExporters(t *testing.T) {
	cleanEnvironment(t)
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Setenv("OTEL_SERVICE_NAME", "local-development")
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.01")
	shutdown, err := Init(context.Background(), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != tp || otel.GetMeterProvider() != mp {
		t.Fatal("unset destination initialized an SDK/exporter")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledOverridesDestination(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "private-invalid")
	shutdown, err := Init(context.Background(), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigurationIsRedacted(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"OTEL_TRACES_EXPORTER", "private-value"},
		{"OTEL_METRICS_EXPORTER", "prometheus"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "private-value"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "private-value"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "http://private-user:private-password@localhost:4318"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "private-value"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "private-value"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "authorization=private-%xx"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "authorization=private-%0Avalue"},
		{"OTEL_RESOURCE_ATTRIBUTES", "private-value"},
		{"OTEL_RESOURCE_ATTRIBUTES", "tenant=private-%xx"},
		{"OTEL_TRACES_SAMPLER", "private-value"},
		{"OTEL_TRACES_SAMPLER_ARG", "private-value"},
		{"OTEL_TRACES_SAMPLER_ARG", "NaN"},
		{"OTEL_TRACES_SAMPLER_ARG", "1.1"},
		{"OTEL_TRACES_SAMPLER_ARG", "-0.1"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
			t.Setenv(tc.key, tc.value)
			_, err := Init(context.Background(), "dev", nil)
			if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unsafe or missing startup error: %v", err)
			}
		})
	}
}

func TestOTLPExportAndGracefulShutdown(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		t.Run(map[bool]string{true: "sampled", false: "unsampled"}[sampled], func(t *testing.T) {
			cleanEnvironment(t)
			var mu sync.Mutex
			traces := &tracespb.ExportTraceServiceRequest{}
			metrics := &metricspb.ExportMetricsServiceRequest{}
			var badHeaders bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer private-token" {
					badHeaders = true
				}
				switch r.URL.Path {
				case "/v1/traces":
					if err := proto.Unmarshal(body, traces); err != nil {
						t.Error(err)
					}
				case "/v1/metrics":
					if err := proto.Unmarshal(body, metrics); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected OTLP path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			// Endpoint alone opts both signals in using autoexport's OTLP default.
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20private-token")
			expectedService := "dscregd"
			if !sampled {
				expectedService = "environment-service"
				t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test,service.name=resource-service")
				t.Setenv("OTEL_SERVICE_NAME", expectedService)
			}
			t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
			ratio := "0"
			if sampled {
				ratio = "1"
			}
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", ratio)
			t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "60000")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "60000")
			var logs bytes.Buffer
			shutdown, err := Init(context.Background(), "v1.2.3", slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ctx, span := otel.Tracer("test").Start(context.Background(), "test upload")
			if span.SpanContext().IsSampled() != sampled {
				t.Fatal("SDK sampler ignored environment")
			}
			histogram, err := otel.Meter("test").Float64Histogram("test.duration", metric.WithUnit("s"))
			if err != nil {
				t.Fatal(err)
			}
			histogram.Record(ctx, .25, metric.WithAttributes(attribute.String("outcome", "created")))
			span.End()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			if err := shutdown(shutdownCtx); err != nil {
				t.Fatal("shutdown must be idempotent", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if badHeaders {
				t.Fatal("OTLP headers not applied")
			}
			if sampled != (len(traces.ResourceSpans) != 0) {
				t.Fatal("trace export sampling incorrect")
			}
			foundHistogram, foundRuntime, foundScheduler := false, false, false
			for _, rm := range metrics.ResourceMetrics {
				attrs := map[string]string{}
				for _, a := range rm.Resource.Attributes {
					attrs[a.Key] = a.Value.GetStringValue()
				}
				if attrs["service.name"] != expectedService || attrs["service.version"] != "v1.2.3" || (!sampled && attrs["deployment.environment.name"] != "test") {
					t.Fatal("resource attributes incorrect", attrs)
				}
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						switch m.Name {
						case "test.duration":
							foundHistogram = m.GetHistogram().DataPoints[0].Count == 1
						case "go.goroutine.count":
							foundRuntime = true
						case "go.schedule.duration":
							foundScheduler = true
						}
					}
				}
			}
			if !foundHistogram || !foundRuntime || !foundScheduler {
				t.Fatalf("metric=%v runtime=%v scheduler=%v", foundHistogram, foundRuntime, foundScheduler)
			}
			if strings.Contains(logs.String(), "private-token") {
				t.Fatal("OTLP credentials leaked")
			}
		})
	}
}

func TestSignalSelection(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	if signalEnabled("TRACES") || !signalEnabled("METRICS") {
		t.Fatal("none does not override endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	if !signalEnabled("TRACES") || signalEnabled("METRICS") {
		t.Fatal("signal-specific endpoint activated wrong signal")
	}
}

func TestExporterOutageDoesNotBlockSpansAndShutdownIsBounded(t *testing.T) {
	cleanEnvironment(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "1")
	t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "8")
	t.Setenv("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "4")
	var logs bytes.Buffer
	shutdown, err := Init(context.Background(), "dev", slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tracer := otel.Tracer("test")
	_, initial := tracer.Start(context.Background(), "initial")
	initial.End()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not start")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, span := tracer.Start(context.Background(), "during outage")
			span.End()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("span creation blocked on exporter or full batch queue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- shutdown(ctx) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("blocked exporter shutdown should report its deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("exporter shutdown ignored bounded context")
	}
}
