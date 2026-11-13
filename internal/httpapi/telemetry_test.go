package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPInstrumentation(t *testing.T) {
	capture := testotel.Setup(t)
	handler, _, _, logs := testHandler(t)
	parentTrace := "0123456789abcdef0123456789abcdef"
	parentSpan := "0123456789abcdef"
	for _, tc := range []struct {
		method, path, route string
		status              int
	}{
		{"GET", "/v1/machines/private-machine", "/v1/machines/{id}", 401},
		{"GET", "/v1/machines/private-machine/reports/private-report?secret=private-query", "/v1/machines/{id}/reports/{reportId}", 401},
		{"POST", "/v1/agent/reports", "/v1/agent/reports", 401},
		{"GET", "/v1/agent/configurations", "/v1/agent/configurations", 401},
		{"HEAD", "/v1/machines/private-machine", "/v1/machines/{id}", 401},
		{"GET", "/health/live", "/health/live", 200},
		{"GET", "/health/ready", "/health/ready", 200},
		{"GET", "/private-unmatched", "", 404},
		{"PATCH", "/v1/agent/configurations", "", 405},
	} {
		logs.Reset()
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("traceparent", "00-"+parentTrace+"-"+parentSpan+"-01")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d, headers %v", w.Code, w.Header())
		}
		spans := capture.Spans.Ended()
		span := spans[len(spans)-1]
		wantName := tc.method
		if tc.route != "" {
			wantName += " " + tc.route
		}
		if span.Name() != wantName || span.SpanKind() != trace.SpanKindServer {
			t.Fatalf("span name/kind = %s/%s", span.Name(), span.SpanKind())
		}
		if span.Parent().SpanID().String() != parentSpan || span.SpanContext().TraceID().String() != parentTrace {
			t.Fatal("incoming trace context lost")
		}
		attrs := attribute.NewSet(span.Attributes()...)
		route, _ := attrs.Value("http.route")
		if route.AsString() != tc.route {
			t.Fatalf("span route = %q", route.AsString())
		}
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.Emit(), "private-") {
				t.Fatalf("private URL exported: %v", attr)
			}
		}
		var log map[string]any
		if err := json.Unmarshal(logs.Bytes(), &log); err != nil {
			t.Fatal(err)
		}
		if log["trace_id"] != parentTrace || log["span_id"] != span.SpanContext().SpanID().String() {
			t.Fatal("request log missing span correlation")
		}
	}
	metrics := capture.Collect(t)
	duration, ok := metrics["http.server.request.duration"].Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("missing standard HTTP duration histogram")
	}
	var count uint64
	found := false
	for _, dp := range duration.DataPoints {
		count += dp.Count
		route, _ := dp.Attributes.Value("http.route")
		status, _ := dp.Attributes.Value("http.response.status_code")
		method, _ := dp.Attributes.Value("http.request.method")
		if route.AsString() == "/v1/machines/{id}" && method.AsString() == http.MethodGet && status.AsInt64() == 401 {
			found = true
		}
		for _, attr := range dp.Attributes.ToSlice() {
			if strings.Contains(attr.Value.Emit(), "private-") {
				t.Fatal("unbounded metric dimension", attr)
			}
		}
	}
	if count != 9 || !found {
		t.Fatalf("duration count = %d, normalized 401=%v", count, found)
	}
}

func TestHTTPErrorTelemetry(t *testing.T) {
	capture := testotel.Setup(t)
	handler, db, _, _ := testHandler(t)
	store := &database.Store{DB: db, Engine: "sqlite"}
	admin, err := store.Bootstrap(context.Background(), "test-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/machines/private-machine", nil)
	r.Header.Set("Authorization", "ApiKey "+admin.Key)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("dependency failure response changed", w.Code)
	}
	spans := capture.Spans.Ended()
	span := spans[len(spans)-1]
	if span.Status().Code != codes.Error {
		t.Fatal("5xx HTTP span missing error status")
	}
	histogram := capture.Collect(t)["http.server.request.duration"].Data.(metricdata.Histogram[float64])
	if len(histogram.DataPoints) != 1 {
		t.Fatal("missing HTTP failure measurement")
	}
	attrs := histogram.DataPoints[0].Attributes
	status, _ := attrs.Value("http.response.status_code")
	kind, _ := attrs.Value("error.type")
	if status.AsInt64() != 503 || kind.AsString() != "503" {
		t.Fatalf("HTTP failure attributes=%v", attrs)
	}
}
