package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	return New(&database.Store{DB: db, Engine: "sqlite"}, reports, 100*time.Millisecond, time.Second, 5*time.Minute, 1<<20, 16<<20, logger), db, reports, logs
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
		{"GET", "/v1/machines", 401, `"code":"unauthorized"`},
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

func await[T any](t *testing.T, results <-chan T) T {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for readiness check")
		var zero T
		return zero
	}
}

func TestConcurrentReadinessRequestsAreHealthy(t *testing.T) {
	_, db, reports, _ := testHandler(t)
	handler := New(&database.Store{DB: db, Engine: "sqlite"}, reports, 2*time.Second, time.Second, 5*time.Minute, 1<<20, 16<<20, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- request(handler, "GET", "/health/ready") }()
	deadline := time.Now().Add(time.Second)
	for db.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("readiness did not start checking the database")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { results <- request(handler, "GET", "/health/ready") }()
	select {
	case response := <-results:
		t.Fatalf("probe returned before dependency check finished: %d %s", response.Code, response.Body)
	case <-time.After(20 * time.Millisecond):
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := await(t, results)
		if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ready\"}\n" {
			t.Fatalf("concurrent readiness: %d %s", response.Code, response.Body)
		}
	}
}

func TestReadinessSharesSuccessAndFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{{"success", nil}, {"failure", errors.New("dependency unavailable")}} {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			started := make(chan struct{}, 16)
			var calls atomic.Int32
			probe := &readinessProbe{timeout: time.Second, check: func(context.Context) error {
				calls.Add(1)
				started <- struct{}{}
				<-release
				return test.failure
			}}
			results := make(chan *probeResult, 16)
			for range 16 {
				go func() { results <- probe.start() }()
			}
			first := await(t, results)
			for range 15 {
				if result := await(t, results); result != first {
					t.Fatal("concurrent callers did not share the same dependency check")
				}
			}
			await(t, started)
			if calls.Load() != 1 {
				t.Fatalf("dependency checks = %d, want 1", calls.Load())
			}
			unblock()
			await(t, first.done)
			if !errors.Is(first.err, test.failure) {
				t.Fatalf("shared result = %v, want %v", first.err, test.failure)
			}
			next := probe.start()
			if next == first {
				t.Fatal("completed result was reused for a new probe")
			}
			await(t, started)
			await(t, next.done)
			if calls.Load() != 2 {
				t.Fatalf("dependency checks = %d, want 2", calls.Load())
			}
		})
	}
}

func TestReadinessCallerCancellationIsIsolated(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	started := make(chan struct{}, 1)
	probe := &readinessProbe{timeout: time.Second, check: func(ctx context.Context) error {
		started <- struct{}{}
		<-release
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		probe.ServeHTTP(response, httptest.NewRequest("GET", "/health/ready", nil).WithContext(ctx))
		results <- response
	}()
	await(t, started)
	shared := probe.start()
	cancel()
	if response := await(t, results); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled request: %d %s", response.Code, response.Body)
	}
	if probe.start() != shared {
		t.Fatal("canceling the initiating caller ended the shared check")
	}
	unblock()
	await(t, shared.done)
	if shared.err != nil {
		t.Fatalf("caller canceled the shared check: %v", shared.err)
	}
}

func TestStalledReadinessCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var calls atomic.Int32
	probe := &readinessProbe{timeout: 20 * time.Millisecond, check: func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}}
	for range 2 {
		start := time.Now()
		response := request(probe, "GET", "/health/ready")
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("stalled probe exceeded its response deadline: %v", elapsed)
		}
		if response.Code != http.StatusServiceUnavailable || response.Body.String() != unavailable {
			t.Fatalf("stalled readiness: %d %s", response.Code, response.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("stalled checks accumulated: %d", calls.Load())
	}
	shared := probe.start()
	unblock()
	await(t, shared.done)
	if !errors.Is(shared.err, context.DeadlineExceeded) {
		t.Fatalf("late completion reported success: %v", shared.err)
	}
	// The old check has actually ended, so a new request may check again.
	if response := request(probe, "GET", "/health/ready"); response.Code != http.StatusOK {
		t.Fatalf("readiness did not recover: %d %s", response.Code, response.Body)
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
