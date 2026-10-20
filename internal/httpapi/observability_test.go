package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBoundedRequestLabels(t *testing.T) {
	handler, _, _, logs := testHandler(t)
	for _, test := range []struct{ method, path, route string }{
		{"GET", "/v1/machines/private-id", "/v1/machines/{id}"},
		{"PUT", "/v1/machines/private-id/configurations/private-name", "/v1/machines/{id}/configurations/{name}"},
		{"GET", "/v1/machines/private-id/configurations/private-name/status", "/v1/machines/{id}/configurations/{name}/status"},
		{"GET", "/v1/machines/private-id/configurations/private-name/report", "/v1/machines/{id}/configurations/{name}/report"},
		{"POST", "/v1/agent/configurations/private-name/report", "/v1/agent/configurations/{name}/report"},
		{"POST", "/v1/agent/reports", "/v1/agent/reports"},
		{"POST", "/v1/agent/register", "/v1/agent/register"},
		{"POST", "/v1/registration-tokens", "/v1/registration-tokens"},
		{"GET", "/v1/machines/private-id/status", "/v1/machines/{id}/status"},
		{"GET", "/v1/machines/private-id/reports", "/v1/machines/{id}/reports"},
		{"GET", "/v1/machines/private-id/reports/private-report", "/v1/machines/{id}/reports/{reportId}"},
		{"GET", "/v1/machines/private-id/reports/private-report/content", "/v1/machines/{id}/reports/{reportId}/content"},
		{"DELETE", "/v1/admins/private-id/api-keys/private-key", "/v1/admins/{id}/api-keys/{keyId}"},
		{"SECRET-METHOD", "/private-path?secret=private-query", "unmatched"},
	} {
		logs.Reset()
		request(handler, test.method, test.path)
		var event map[string]any
		if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		method := test.method
		if method == "SECRET-METHOD" {
			method = "other"
		}
		if event["route"] != test.route || event["method"] != method || event["status"] == nil || event["duration"] == nil {
			t.Fatalf("invalid request signal: %s", logs)
		}
		if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "SECRET-METHOD") {
			t.Fatal("untrusted request data in labels")
		}
	}
}

func TestResponseStatusTracksFirstFinalHeader(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		response := &responseWriter{ResponseWriter: recorder, status: 200}
		if implicit {
			_, _ = response.Write([]byte("body"))
		} else {
			response.WriteHeader(201)
		}
		response.WriteHeader(503)
		if response.status != recorder.Code || response.Unwrap() != recorder {
			t.Fatal("logged status differs from actual response")
		}
	}
}
