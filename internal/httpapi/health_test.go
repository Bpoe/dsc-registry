package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/service"
)

func testHandler(t *testing.T) (http.Handler, *sql.DB, *reportstore.Store, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	reports, err := reportstore.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	db, err := database.Open(context.Background(), filepath.Join(root, "registry.db"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	return New(service.New(&database.Store{DB: db, Engine: "sqlite"}, reports, nil, 16<<20, logger), time.Second, 5*time.Minute, 1<<20, 16<<20, logger), db, reports, logs
}

func request(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "ApiKey dsca_sensitive.secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	return response
}

func TestHealthRoutes(t *testing.T) {
	handler, _, _, logs := testHandler(t)
	for _, test := range []struct {
		method string
		path   string
		status int
		body   string
	}{
		{"GET", "/health/live", 200, `{"status":"live"}`},
		{"GET", "/health/ready", 200, `{"status":"ready"}`},
		{"GET", "/v1/machines", 401, `"code":"unauthorized"`},
	} {
		response := request(handler, test.method, test.path)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
			t.Fatalf("%s %s: status=%d, body=%s", test.method, test.path, response.Code, response.Body)
		}
		if response.Header().Get("Content-Type") != "application/json" ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("health/error headers: %v", response.Header())
		}
	}
	if strings.Contains(logs.String(), "sensitive") {
		t.Fatalf("logs contain untrusted path, query or credentials: %s", logs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil ||
			entry["msg"] != "http request" || entry["status"] == nil || entry["duration"] == nil {
			t.Fatalf("invalid structured request log %q: %v", line, err)
		}
	}
}

func TestHealthIgnoresDependencies(t *testing.T) {
	// Nil dependencies prove that neither route even accesses the stores.
	handler := New(service.New(nil, nil, nil, 1024, slog.New(slog.NewJSONHandler(io.Discard, nil))), time.Second, time.Minute, 1024, 1024, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	for _, path := range []string{"/health/live", "/health/ready"} {
		if response := request(handler, "GET", path); response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
	}
}

func TestDependencyFailuresDoNotAffectHealth(t *testing.T) {
	for _, dependency := range []string{"database", "report_storage"} {
		t.Run(dependency, func(t *testing.T) {
			handler, db, reports, _ := testHandler(t)
			if dependency == "database" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := reports.Close(); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []struct{ path, body string }{
				{"/health/live", "{\"status\":\"live\"}\n"},
				{"/health/ready", "{\"status\":\"ready\"}\n"},
			} {
				response := request(handler, "GET", endpoint.path)
				if response.Code != http.StatusOK || response.Body.String() != endpoint.body {
					t.Fatalf("%s with failed %s: %d %s", endpoint.path, dependency, response.Code, response.Body)
				}
				if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("health headers changed", response.Header())
				}
			}
		})
	}
}

func TestHeadOverHTTP(t *testing.T) {
	handler, _, _, _ := testHandler(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/health/live", "/health/ready"} {
		response, err := client.Head(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || len(body) != 0 ||
			response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("HEAD %s: status=%d, body=%q, error=%v", path, response.StatusCode, body, readErr)
		}
	}
}
