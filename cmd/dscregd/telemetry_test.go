package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
)

func TestRunServesRequestsDuringLogExporterOutage(t *testing.T) {
	const requestSecret = "private-request-token"
	const responseSecret = "private-backend-diagnostic"
	const exportWarning = "telemetry export or collection failed"
	entered, release := make(chan struct{}), make(chan struct{})
	var first, unblock sync.Once
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Error("unexpected signal exported")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if bytes.Contains(body, []byte(requestSecret)) || bytes.Contains(body, []byte(responseSecret)) {
			t.Error("exported logs disclosed a secret")
		}
		if bytes.Contains(body, []byte(exportWarning)) {
			t.Error("exporter diagnostics were routed back to the exporter")
		}
		first.Do(func() {
			close(entered)
			<-release
		})
		http.Error(w, responseSecret, http.StatusBadRequest)
	}))
	defer collector.Close()
	defer unblock.Do(func() { close(release) })
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", collector.URL+"/v1/logs")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "none")
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "1")
	t.Setenv("OTEL_BLRP_MAX_QUEUE_SIZE", "8")
	t.Setenv("OTEL_BLRP_MAX_EXPORT_BATCH_SIZE", "4")

	settings := config.Defaults()
	settings.ListenAddress = "127.0.0.1:0"
	settings.DatabaseDSN = filepath.Join(t.TempDir(), "registry.db")
	settings.ReportRoot = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := make(chan string, 1)
	var local bytes.Buffer
	logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(&local, nil), address: address})
	done := make(chan error, 1)
	go func() { done <- run(ctx, settings, logger) }()
	base := "http://" + receive(t, address)
	receive(t, entered)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	for range 32 {
		req, err := http.NewRequest(http.MethodPost, base+"/v1/agent/register", strings.NewReader(requestSecret))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+requestSecret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("request status during exporter outage = %d", resp.StatusCode)
		}
	}
	unblock.Do(func() { close(release) })
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	output := local.String()
	if !strings.Contains(output, exportWarning) {
		t.Fatal("local exporter warning missing")
	}
	if count := strings.Count(output, `"msg":"http request"`); count != 32 {
		t.Fatalf("local request logs = %d; want 32", count)
	}
	if strings.Contains(output, requestSecret) || strings.Contains(output, responseSecret) {
		t.Fatal("local logs disclosed a secret")
	}
}
