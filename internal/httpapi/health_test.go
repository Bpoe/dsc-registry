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
	return New(db, reports, 100*time.Millisecond, logger), db, reports, logs
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
		{"POST", "/health/ready", 405, `"code":"method_not_allowed"`},
		{"DELETE", "/health/live", 405, `"code":"method_not_allowed"`},
		{"GET", "/v1/machines", 404, `"code":"not_found"`},
		{"GET", "/unknown-sensitive?key=also-sensitive", 404, `"code":"not_found"`},
	} {
		response := request(handler, test.method, test.path)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
			t.Fatalf("%s %s: status=%d, body=%s", test.method, test.path, response.Code, response.Body)
		}
		if response.Header().Get("Content-Type") != "application/json" ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("health/error headers: %v", response.Header())
		}
		if test.status == 405 && response.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("Allow = %q", response.Header().Get("Allow"))
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

func TestDependencyFailuresDoNotAffectLiveness(t *testing.T) {
	for _, dependency := range []string{"database", "reports"} {
		t.Run(dependency, func(t *testing.T) {
			handler, db, reports, _ := testHandler(t)
			if dependency == "database" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := reports.Close(); err != nil {
				t.Fatal(err)
			}
			response := request(handler, "GET", "/health/ready")
			if response.Code != http.StatusServiceUnavailable || response.Body.String() != unavailable {
				t.Fatalf("readiness: %d %s", response.Code, response.Body)
			}
			if response := request(handler, "GET", "/health/live"); response.Code != http.StatusOK {
				t.Fatalf("liveness: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestReadinessTimeout(t *testing.T) {
	// Hold the only database connection so Ping must wait for the probe deadline.
	handler, db, _, _ := testHandler(t)
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	start := time.Now()
	response := request(handler, "GET", "/health/ready")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readiness response exceeded its bound: %v", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != unavailable {
		t.Fatalf("readiness timeout: %d %s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("timeout Content-Type = %q", response.Header().Get("Content-Type"))
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
