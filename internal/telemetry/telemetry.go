// Package telemetry owns process-level OpenTelemetry SDK setup. Instrumented
// packages use the OpenTelemetry APIs directly.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	runtimeotel "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/net/http/httpguts"
)

func init() {
	// autoexport attaches the official runtime histogram producer to its
	// periodic reader; this adds scheduler metrics without a second reader.
	autoexport.WithFallbackMetricProducer(func(context.Context) (sdkmetric.Producer, error) {
		return runtimeotel.NewProducer(), nil
	})
}

// Init is called once by the server, before constructing dependencies. An
// exporter selection or endpoint opts a signal in; resource/sampling settings
// alone never cause a connection to the default localhost OTLP destination.
// Init returns the logger to pass to server dependencies. The caller must
// invoke shutdown after HTTP requests and event work drain.
func Init(ctx context.Context, version string, logger *slog.Logger) (configuredLogger *slog.Logger, shutdown func(context.Context) error, err error) {
	shutdown = func(context.Context) error { return nil }
	if logger == nil {
		logger = slog.Default()
	}
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return logger, shutdown, nil
	}
	if err := validateEnvironment(); err != nil {
		return logger, shutdown, err
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	traces, metrics := signalEnabled("TRACES"), signalEnabled("METRICS")
	logs := signalEnabled("LOGS")
	if !traces && !metrics && !logs {
		return logger, shutdown, nil
	}
	// Keep diagnostics on the original logger so exporter failures cannot
	// enqueue more OTLP records. Diagnostic errors may contain secrets.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { logger.Warn("telemetry export or collection failed", "reason", "otel_failure") }))
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "dscregd"), attribute.String("service.version", version)),
		resource.WithTelemetrySDK(), resource.WithFromEnv())
	if err != nil {
		return logger, shutdown, errors.New("invalid OTEL_RESOURCE_ATTRIBUTES")
	}
	var tp *sdktrace.TracerProvider
	var mp *sdkmetric.MeterProvider
	var lp *sdklog.LoggerProvider
	var shutdownOnce sync.Once
	var shutdownErr error
	shutdown = func(ctx context.Context) error {
		shutdownOnce.Do(func() {
			var traceErr, metricErr, logErr error
			var work sync.WaitGroup
			if mp != nil {
				work.Go(func() { metricErr = mp.Shutdown(ctx) })
			}
			if tp != nil {
				work.Go(func() { traceErr = tp.Shutdown(ctx) })
			}
			if lp != nil {
				work.Go(func() { logErr = lp.Shutdown(ctx) })
			}
			work.Wait()
			shutdownErr = errors.Join(traceErr, metricErr, logErr)
		})
		return shutdownErr
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdown(cleanup)
		}
	}()
	if traces {
		exporter, exportErr := autoexport.NewSpanExporter(ctx)
		if exportErr != nil {
			return logger, shutdown, errors.New("cannot initialize OTEL_TRACES_EXPORTER; check OTLP traces settings")
		}
		// SDK batching is bounded and nonblocking by default; standard OTEL_BSP_*
		// variables configure queue/batch sizes and delays.
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	}
	if metrics {
		reader, exportErr := autoexport.NewMetricReader(ctx)
		if exportErr != nil {
			return logger, shutdown, errors.New("cannot initialize OTEL_METRICS_EXPORTER; check OTLP metrics settings")
		}
		mp = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader), sdkmetric.WithCardinalityLimit(2000))
		if runtimeErr := runtimeotel.Start(runtimeotel.WithMeterProvider(mp), runtimeotel.WithMinimumReadMemStatsInterval(15*time.Second)); runtimeErr != nil {
			return logger, shutdown, errors.New("cannot initialize Go runtime instrumentation")
		}
	}
	configuredLogger = logger
	if logs {
		exporter, exportErr := autoexport.NewLogExporter(ctx)
		if exportErr != nil {
			return logger, shutdown, errors.New("cannot initialize OTEL_LOGS_EXPORTER; check OTLP logs settings")
		}
		lp = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
		bridge := otelslog.NewHandler("dscregd", otelslog.WithLoggerProvider(lp))
		configuredLogger = slog.New(slog.NewMultiHandler(logger.Handler(), bridge))
	}
	// Publish globals only after all providers have initialized successfully.
	if tp != nil {
		otel.SetTracerProvider(tp)
	}
	if mp != nil {
		otel.SetMeterProvider(mp)
	}
	return configuredLogger, shutdown, nil
}

func signalEnabled(signal string) bool {
	if exporter := os.Getenv("OTEL_" + signal + "_EXPORTER"); exporter != "" {
		return exporter != "none"
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

// The SDK handles valid sampler configuration and exporters handle valid OTLP
// configuration. Validate values which otherwise fall back silently or could
// start an out-of-scope Prometheus listener. Never include environment values
// in errors: endpoints and headers may contain credentials.
func validateEnvironment() error {
	if value := os.Getenv("OTEL_SDK_DISABLED"); value != "" && value != "true" && value != "false" {
		return errors.New("invalid OTEL_SDK_DISABLED; use true or false")
	}
	if value := os.Getenv("OTEL_METRICS_PRODUCERS"); value != "" && value != "none" {
		return errors.New("OTEL_METRICS_PRODUCERS supports only none; the default is the Go runtime producer")
	}
	if attrs := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); attrs != "" {
		for _, pair := range strings.Split(attrs, ",") {
			key, value, ok := strings.Cut(pair, "=")
			_, err := url.PathUnescape(strings.TrimSpace(value))
			if !ok || strings.TrimSpace(key) == "" || err != nil {
				return errors.New("invalid OTEL_RESOURCE_ATTRIBUTES; require comma-separated key=value pairs")
			}
		}
	}
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		key := "OTEL_" + signal + "_EXPORTER"
		switch os.Getenv(key) {
		case "", "none", "otlp", "console":
		default:
			return fmt.Errorf("invalid %s; use none, otlp, or console", key)
		}
	}
	for _, suffix := range []string{"", "_TRACES", "_METRICS", "_LOGS"} {
		prefix := "OTEL_EXPORTER_OTLP" + suffix
		if protocol := os.Getenv(prefix + "_PROTOCOL"); protocol != "" && protocol != "grpc" && protocol != "http/protobuf" {
			return fmt.Errorf("invalid %s_PROTOCOL; use grpc or http/protobuf", prefix)
		}
		if endpoint := os.Getenv(prefix + "_ENDPOINT"); endpoint != "" {
			u, err := url.Parse(endpoint)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("invalid %s_ENDPOINT; require an http(s) URL without credentials, query, or fragment", prefix)
			}
		}
		if headers := os.Getenv(prefix + "_HEADERS"); headers != "" {
			for _, header := range strings.Split(headers, ",") {
				key, value, ok := strings.Cut(header, "=")
				decoded, err := url.PathUnescape(strings.TrimSpace(value))
				if !ok || !httpguts.ValidHeaderFieldName(strings.TrimSpace(key)) || !httpguts.ValidHeaderFieldValue(decoded) || err != nil {
					return fmt.Errorf("invalid %s_HEADERS; require comma-separated key=value pairs", prefix)
				}
			}
		}
	}
	sampler := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	switch sampler {
	case "", "always_on", "always_off", "parentbased_always_on", "parentbased_always_off":
	case "traceidratio", "parentbased_traceidratio":
		if arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); arg != "" {
			value, err := strconv.ParseFloat(arg, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				return errors.New("invalid OTEL_TRACES_SAMPLER_ARG; require a finite number between 0 and 1")
			}
		}
	default:
		return errors.New("invalid OTEL_TRACES_SAMPLER; use a standard Go SDK sampler")
	}
	return nil
}
