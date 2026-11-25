package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/telemetry"
	"github.com/Bpoe/dsc-registry/internal/testotel"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

func TestHTTPResourceTelemetry(t *testing.T) {
	capture := testotel.Setup(t)
	handler, db, files, logs := testHandler(t)
	store := &database.Store{DB: db, Engine: "sqlite"}
	ctx := context.Background()
	admin, err := store.Bootstrap(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	lastRequest := func(route string, status int, want ...attribute.KeyValue) sdktrace.ReadOnlySpan {
		t.Helper()
		spans := capture.Spans.Ended()
		span := spans[len(spans)-1]
		attrs := attribute.NewSet(span.Attributes()...)
		routeAttr, _ := attrs.Value("http.route")
		pathAttr, _ := attrs.Value("url.path")
		statusAttr, _ := attrs.Value("http.response.status_code")
		if span.SpanKind() != trace.SpanKindServer || routeAttr.AsString() != route ||
			pathAttr.AsString() != route || statusAttr.AsInt64() != int64(status) {
			t.Fatalf("unexpected HTTP span %q: %v", span.Name(), attrs)
		}
		assertResourceAttributes(t, span, want...)
		return span
	}
	machine := decodeResponse[database.Machine](t,
		apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"traced-host"}`, 201))
	machineID := telemetry.MachineID.String(machine.ID)
	machineName := telemetry.MachineName.String(machine.Name)
	lastRequest("/v1/machines", 201, machineID, machineName)
	base := "/v1/machines/" + machine.ID
	apiRequest(t, handler, admin.Key, "GET", base, "", 200)
	lastRequest("/v1/machines/{id}", 200, machineID, machineName)
	key := decodeResponse[database.IssuedKey](t,
		apiRequest(t, handler, admin.Key, "POST", base+"/api-keys", `{}`, 201))
	lastRequest("/v1/machines/{id}/api-keys", 201, machineID)
	apiRequest(t, handler, admin.Key, "GET", "/v1/admins/"+admin.AdminID+"/api-keys", "", 200)
	lastRequest("/v1/admins/{id}/api-keys", 200)

	const configRoute = "/v1/machines/{id}/configurations/{name}"
	configPath := base + "/configurations/docker"
	configName := telemetry.ConfigurationName.String("docker")
	config := decodeResponse[database.ConfigurationResult](t,
		apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201))
	lastRequest(configRoute, 201, machineID, configName)
	poll := agentRequest(handler, key.Key)
	if poll.Code != http.StatusOK || poll.Header().Get("ETag") == "" {
		t.Fatalf("agent polling response changed: %d %v", poll.Code, poll.Header())
	}
	lastRequest("/v1/agent/configurations", 200, machineID)
	if cached := agentRequest(handler, key.Key, poll.Header().Get("ETag")); cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatalf("agent conditional response changed: %d %s", cached.Code, cached.Body)
	}
	lastRequest("/v1/agent/configurations", 304, machineID)

	body := reportBody(config.Revision, uuid.NewString(), rawSuccessReport)
	published := decodeResponse[database.ReportMetadata](t,
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201))
	reportID := telemetry.ReportID.String(published.ID)
	publishedSpan := lastRequest("/v1/agent/reports", 201, machineID, configName, reportID)
	_, path, err := store.GetReport(ctx, machine.ID, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := telemetry.ReportPath.String(path)
	operation := func(parent sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
		t.Helper()
		for _, span := range capture.Spans.Ended() {
			if span.Name() == name && span.Parent().SpanID() == parent.SpanContext().SpanID() {
				return span
			}
		}
		t.Fatalf("span %q missing under %q", name, parent.Name())
		return nil
	}
	publication := operation(publishedSpan, "Service.PublishReport")
	assertResourceAttributes(t, publication, machineID, configName, reportID)
	assertResourceAttributes(t, operation(publication, "Publish metadata"), machineID, configName, reportID)
	assertResourceAttributes(t, operation(publication, "Report storage write"), reportPath)
	retry := decodeResponse[database.ReportMetadata](t,
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200))
	if retry.ID != published.ID {
		t.Fatal("retry returned a different report")
	}
	lastRequest("/v1/agent/reports", 200, machineID, configName, reportID)
	conflict := strings.Replace(body, "private-dsc-result", "private-changed-result", 1)
	apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", conflict, 409)
	lastRequest("/v1/agent/reports", 409, machineID, configName, reportID)
	drainHTTPReportFiles(t, store, files, nil, 0)

	const reportRoute = "/v1/machines/{id}/reports/{reportId}"
	reportURL := base + "/reports/" + published.ID
	apiRequest(t, handler, admin.Key, "GET", reportURL, "", 200)
	lastRequest(reportRoute, 200, machineID, configName, reportID)
	apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 200)
	lastRequest(configRoute+"/status", 200, machineID, configName, reportID)
	if got := apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 200); got.Body.String() != rawSuccessReport {
		t.Fatal("retrieved report content changed")
	}
	lastRequest(configRoute+"/report", 200, machineID, configName, reportID)
	if err := files.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ url, route string }{
		{reportURL + "/content", reportRoute + "/content"},
		{configPath + "/report", configRoute + "/report"},
	} {
		logs.Reset()
		response := apiRequest(t, handler, admin.Key, "GET", target.url, "", 503)
		if !strings.Contains(response.Body.String(), `"code":"report_storage_unavailable"`) {
			t.Fatal("report storage error response changed", response.Body)
		}
		failed := lastRequest(target.route, 503, machineID, configName, reportID)
		read := operation(failed, "Report storage read")
		assertResourceAttributes(t, read, machineID, configName, reportID, reportPath)
		if read.Status().Code != codes.Error || failed.Status().Code != codes.Error {
			t.Fatal("report read error status missing")
		}
		sc := failed.SpanContext()
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		var requestLog map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &requestLog); err != nil ||
			requestLog["trace_id"] != sc.TraceID().String() || requestLog["span_id"] != sc.SpanID().String() {
			t.Fatal("report storage failure lost request log correlation", err)
		}
		for _, value := range []string{machine.ID, published.ID, "docker", path} {
			if strings.Contains(logs.String(), value) {
				t.Fatalf("resource value %q leaked into logs", value)
			}
		}
	}

	apiRequest(t, handler, "", "GET", reportURL+"/content", "", 401)
	lastRequest(reportRoute+"/content", 401)
	apiRequest(t, handler, key.Key, "GET", configPath, "", 403)
	lastRequest(configRoute, 403)
	apiRequest(t, handler, admin.Key, "GET", base+"/reports/private-invalid-report/content", "", 400)
	lastRequest(reportRoute+"/content", 400)

	token := decodeResponse[database.RegistrationToken](t,
		apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", `{}`, 201))
	registered := registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"registered-host"}`, "Bearer "+token.Token)
	if registered.Code != 201 {
		t.Fatal("registration response changed", registered.Code)
	}
	agent := decodeResponse[database.RegisteredMachine](t, registered)
	lastRequest("/v1/agent/register", 201, telemetry.MachineID.String(agent.ID), telemetry.MachineName.String(agent.Name))
	secrets := []string{admin.Key, key.Key, token.Token, agent.APIKey, "private-dsc-result", "private-changed-result", "private-invalid-report", base}
	for _, span := range capture.Spans.Ended() {
		for _, attr := range span.Attributes() {
			for _, secret := range secrets {
				if strings.Contains(attr.Value.Emit(), secret) {
					t.Fatalf("private data exported in span %q attribute %q", span.Name(), attr.Key)
				}
			}
		}
	}
	assertMetricResourceIsolation(t, capture.Collect(t), machine.ID, machine.Name, "docker", published.ID, path)
}

func assertResourceAttributes(t *testing.T, span sdktrace.ReadOnlySpan, want ...attribute.KeyValue) {
	t.Helper()
	expected := attribute.NewSet(want...)
	got := attribute.NewSet(span.Attributes()...)
	for _, key := range []attribute.Key{
		"dscreg.machine.id", "dscreg.machine.name", "dscreg.configuration.name", "dscreg.report.id", "dscreg.report.path",
	} {
		wantValue, wantPresent := expected.Value(key)
		if gotValue, gotPresent := got.Value(key); wantPresent != gotPresent || wantValue != gotValue {
			t.Errorf("span %q attribute %q = %v, want %v", span.Name(), key, gotValue, wantValue)
		}
	}
}

func assertMetricResourceIsolation(t *testing.T, metrics map[string]metricdata.Metrics, identifiers ...string) {
	t.Helper()
	for _, name := range []string{
		"http.server.request.duration", "db.client.operation.duration", "dscreg.report.ingest.duration",
		"dscreg.report.publish.duration", "dscreg.report.storage.operation.duration", "dscreg.report.cleanup.attempts",
	} {
		if _, ok := metrics[name]; !ok {
			t.Errorf("missing measurement %q", name)
		}
	}
	for name, metric := range metrics {
		check := func(attrs attribute.Set) {
			for _, a := range attrs.ToSlice() {
				switch a.Key {
				case telemetry.MachineID, telemetry.MachineName, telemetry.ConfigurationName, telemetry.ReportID, telemetry.ReportPath:
					t.Errorf("resource attribute %q on metric %q", a.Key, name)
				}
				for _, id := range identifiers {
					if strings.Contains(a.Value.Emit(), id) {
						t.Errorf("resource value %q on metric %q", id, name)
					}
				}
			}
		}
		switch data := metric.Data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				check(point.Attributes)
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				check(point.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, point := range data.DataPoints {
				check(point.Attributes)
			}
		case metricdata.Gauge[int64]:
			for _, point := range data.DataPoints {
				check(point.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, point := range data.DataPoints {
				check(point.Attributes)
			}
		default:
			t.Errorf("unexpected metric aggregation %T for %q", data, name)
		}
	}
}
