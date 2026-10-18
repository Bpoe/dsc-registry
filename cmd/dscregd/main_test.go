package main

import (
	"context"
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
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, config.Defaults(), testLogger()) }()
	result := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
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
	settings := config.Defaults()
	settings.ShutdownTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, settings, testLogger()) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
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
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		testRunStartupHealthShutdownAndRestart(t, engine, dsn)
	})
}

func testRunStartupHealthShutdownAndRestart(t *testing.T, engine, dsn string) {
	root := t.TempDir()
	settings := config.Defaults()
	settings.DevelopmentHTTP = true
	settings.ListenAddress = "127.0.0.1:0"
	settings.DatabaseEngine = engine
	settings.DatabaseDSN = dsn
	settings.ReportRoot = filepath.Join(root, "reports")
	if err := os.Mkdir(settings.ReportRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		address := make(chan string, 1)
		logger := slog.New(listeningLog{Handler: testLogger().Handler(), address: address})
		done := make(chan error, 1)
		go func() { done <- run(ctx, settings, logger) }()
		t.Cleanup(cancel)
		host := receive(t, address)
		client := &http.Client{Timeout: 3 * time.Second}
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
		store, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		db := store.DB
		if _, err := db.Exec("INSERT INTO machines VALUES ($1, $2, 0, $3) ON CONFLICT DO NOTHING",
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
	if err := run(context.Background(), settings, testLogger()); err == nil {
		t.Fatal("implicit HTTP mode accepted")
	}
	settings.DevelopmentHTTP = true
	settings.ReportRoot = filepath.Join(t.TempDir(), "missing")
	if err := run(context.Background(), settings, testLogger()); err == nil {
		t.Fatal("missing report root accepted")
	}
}
