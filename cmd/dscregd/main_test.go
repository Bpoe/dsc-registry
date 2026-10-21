package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server")
		var zero T
		return zero
	}
}

func TestGracefulShutdownDrainsRequests(t *testing.T) {
	for _, mode := range []string{"http", "https"} {
		t.Run(mode, func(t *testing.T) { testGracefulShutdownDrainsRequests(t, mode == "https") })
	}
}

func testGracefulShutdownDrainsRequests(t *testing.T, nativeTLS bool) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = io.WriteString(w, "completed")
		case <-r.Context().Done():
		}
	})}
	defer server.Close()
	settings, client, scheme := testServerTransport(t, server, nativeTLS)
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, settings, testLogger()) }()
	result := make(chan error, 1)
	go func() {
		response, err := client.Get(scheme + "://" + listener.Addr().String())
		if err == nil {
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			err = readErr
			if err == nil && (response.StatusCode != http.StatusOK || string(body) != "completed") {
				err = errors.New("in-flight response was not preserved")
			}
		}
		result <- err
	}()
	receive(t, entered)
	cancel()
	receive(t, shuttingDown)
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before draining request: %v", err)
	default:
	}
	close(release)
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDeadlineClosesRequests(t *testing.T) {
	for _, mode := range []string{"http", "https"} {
		t.Run(mode, func(t *testing.T) { testShutdownDeadlineClosesRequests(t, mode == "https") })
	}
}

func testShutdownDeadlineClosesRequests(t *testing.T, nativeTLS bool) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, client, scheme := testServerTransport(t, server, nativeTLS)
	settings.ShutdownTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, settings, testLogger()) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := client.Get(scheme + "://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
	}()
	receive(t, entered)
	cancel()
	if err := receive(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown deadline: %v", err)
	}
	receive(t, requestDone)
}

type listeningLog struct {
	slog.Handler
	address chan string
}

func (h listeningLog) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "server listening" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "address" {
				h.address <- attr.Value.String()
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, record)
}

func TestRunStartupHealthShutdownAndRestart(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:0", "localhost:0"} {
		t.Run(address, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				testRunStartupHealthShutdownAndRestart(t, engine, dsn, address)
			})
		})
	}
}

func testRunStartupHealthShutdownAndRestart(t *testing.T, engine, dsn, listenAddress string) {
	root := t.TempDir()
	settings := config.Defaults()
	settings.ListenAddress = listenAddress
	settings.DatabaseEngine = engine
	settings.DatabaseDSN = dsn
	settings.ReportRoot = filepath.Join(root, "reports")
	if err := os.Mkdir(settings.ReportRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"listenAddress": settings.ListenAddress,
		"database":      map[string]string{"engine": engine, "dsn": dsn},
		"reportRoot":    settings.ReportRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err = config.Parse([]string{"-config", configPath}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		address := make(chan string, 1)
		var output bytes.Buffer
		logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(&output, nil), address: address})
		done := make(chan error, 1)
		go func() { done <- run(ctx, settings, logger) }()
		t.Cleanup(cancel)
		host := receive(t, address)
		if listenAddress == "0.0.0.0:0" {
			_, port, err := net.SplitHostPort(host)
			if err != nil {
				t.Fatal(err)
			}
			host = net.JoinHostPort("127.0.0.1", port)
		}
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		for _, path := range []string{"/health/live", "/health/ready"} {
			response, err := client.Get("http://" + host + path)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "status") {
				cancel()
				t.Fatalf("%s: response=%s, error=%v", path, body, err)
			}
		}
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
		assertHTTPTransportWarnings(t, output.Bytes(), 1)
		assertListenerClosed(t, host)
		store, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		db := store.DB
		if _, err := db.Exec(`INSERT INTO machines(id, name, configuration_revision, created_at, name_folded)
			VALUES ($1, $2, 0, $3, $2) ON CONFLICT (id) DO NOTHING`,
			"550e8400-e29b-41d4-a716-446655440000", "persisted", "2026-10-18T00:00:00Z"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		var count int
		err = db.QueryRow("SELECT COUNT(*) FROM machines").Scan(&count)
		closeErr := db.Close()
		if err != nil || closeErr != nil || count != 1 {
			t.Fatalf("restart persistence: count=%d, error=%v, close=%v", count, err, closeErr)
		}
	}
}

func TestRunRejectsInvalidStartup(t *testing.T) {
	settings := config.Defaults()
	settings.ReportRoot = filepath.Join(t.TempDir(), "missing")
	if err := run(context.Background(), settings, testLogger()); err == nil {
		t.Fatal("missing report root accepted")
	}
}

func assertHTTPTransportWarnings(t *testing.T, output []byte, want int) {
	t.Helper()
	const message = "HTTP transport is unencrypted; API keys and registration tokens may be exposed in transit"
	decoder := json.NewDecoder(bytes.NewReader(output))
	count := 0
	for {
		var record struct {
			Level   string `json:"level"`
			Message string `json:"msg"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record.Message == message {
			if record.Level != "WARN" {
				t.Fatalf("HTTP transport warning level = %s", record.Level)
			}
			count++
		}
	}
	if count != want {
		t.Fatalf("HTTP startup warnings = %d; want %d", count, want)
	}
}

func TestRunHTTPAuthenticatedAPIs(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		settings, err := config.Parse([]string{
			"-listen", "127.0.0.1:0", "-database-engine", engine,
			"-database-dsn", dsn, "-report-root", t.TempDir(),
		}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		db, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		admin, err := db.Bootstrap(context.Background(), "operator")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		address := make(chan string, 1)
		var output bytes.Buffer
		logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(&output, nil), address: address})
		done := make(chan error, 1)
		go func() { done <- run(ctx, settings, logger) }()
		base := "http://" + receive(t, address)
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		request := func(method, path, authorization, body string, status int) []byte {
			t.Helper()
			req, err := http.NewRequest(method, base+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", authorization)
			req.Header.Set("Content-Type", "application/json")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != status {
				t.Fatalf("%s %s: status=%d, error=%v", method, path, response.StatusCode, err)
			}
			if response.TLS != nil || response.Request.URL.Scheme != "http" {
				t.Fatal("HTTP request switched to HTTPS")
			}
			return data
		}
		adminAuth := "ApiKey " + admin.Key
		request(http.MethodGet, "/v1/machines", "", "", http.StatusUnauthorized)
		request(http.MethodGet, "/v1/machines", "ApiKey invalid", "", http.StatusUnauthorized)
		request(http.MethodGet, "/v1/machines", adminAuth, "", http.StatusOK)
		request(http.MethodPost, "/v1/agent/register", "", `{"name":"http-machine"}`, http.StatusUnauthorized)
		request(http.MethodPost, "/v1/agent/register", adminAuth, `{"name":"http-machine"}`, http.StatusUnauthorized)
		request(http.MethodPost, "/v1/agent/register", "Bearer "+admin.Key, `{"name":"http-machine"}`, http.StatusUnauthorized)
		var token struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(request(http.MethodPost, "/v1/registration-tokens", adminAuth, `{}`, http.StatusCreated), &token); err != nil || token.Token == "" {
			t.Fatal("HTTP token issuance failed")
		}
		var machine struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.Unmarshal(request(http.MethodPost, "/v1/agent/register", "Bearer "+token.Token, `{"name":"http-machine"}`, http.StatusCreated), &machine); err != nil || machine.APIKey == "" {
			t.Fatal("HTTP machine registration failed")
		}
		machineAuth := "ApiKey " + machine.APIKey
		request(http.MethodGet, "/v1/agent/configurations", machineAuth, "", http.StatusOK)
		request(http.MethodGet, "/v1/agent/configurations", "", "", http.StatusUnauthorized)
		request(http.MethodGet, "/v1/agent/configurations", adminAuth, "", http.StatusForbidden)
		request(http.MethodGet, "/v1/agent/configurations", "Bearer "+token.Token, "", http.StatusUnauthorized)
		request(http.MethodGet, "/v1/agent/configurations", "ApiKey "+token.Token, "", http.StatusUnauthorized)
		request(http.MethodGet, "/v1/machines", machineAuth, "", http.StatusForbidden)
		request(http.MethodPost, "/v1/registration-tokens", machineAuth, `{}`, http.StatusForbidden)
		request(http.MethodPost, "/v1/agent/register", "Bearer "+machine.APIKey, `{"name":"another-machine"}`, http.StatusUnauthorized)
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
		assertHTTPTransportWarnings(t, output.Bytes(), 1)
		for _, secret := range []string{admin.Key, machine.APIKey, token.Token} {
			if strings.Contains(output.String(), secret) {
				t.Fatal("HTTP startup or request logs disclosed credentials")
			}
		}
	})
}
