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
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
)

func TestRunServesDuringBrokerOutageAndStopsPublisher(t *testing.T) {
	// A peer that accepts TCP but never negotiates AMQP exercises cancellation
	// of a real in-flight connection attempt during application shutdown.
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	connected, disconnected := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(disconnected)
		conn, err := broker.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(connected)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
	}()
	settings := config.Defaults()
	settings.ListenAddress = "127.0.0.1:0"
	settings.DatabaseDSN = filepath.Join(t.TempDir(), "registry.db")
	settings.ReportRoot = t.TempDir()
	eventSettings := config.EventingDefaults()
	eventSettings.Endpoint, eventSettings.Address = "amqp://"+broker.Addr().String(), "opaque"
	eventSettings.ConnectTimeout, eventSettings.PublishTimeout = time.Hour, time.Hour
	eventSettings.ShutdownTimeout = 30 * time.Millisecond
	settings.Eventing = &eventSettings
	db, err := database.Connect(context.Background(), "sqlite", settings.DatabaseDSN, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.Bootstrap(context.Background(), "test")
	db.DB.Close()
	if err != nil {
		t.Fatal(err)
	}
	address := make(chan string, 1)
	var logs bytes.Buffer
	logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(&logs, nil), address: address})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, settings, logger) }()
	host := receive(t, address)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	for i, name := range []string{"event-host", "queued-host"} {
		request, err := http.NewRequest(http.MethodPost, "http://"+host+"/v1/machines", strings.NewReader(`{"name":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "ApiKey "+admin.Key)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("HTTP waited for broker communication", err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusCreated {
			t.Fatal("broker outage changed mutation response", response.StatusCode)
		}
		if i == 0 {
			receive(t, connected)
		}
	}
	response, err := client.Get("http://" + host + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatal("broker outage changed readiness")
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal("broker outage failed an otherwise clean shutdown", err)
	}
	if !strings.Contains(logs.String(), `"outcome":"timeout","pending":1`) ||
		!strings.Contains(logs.String(), `"reason":"best_effort_event_loss"`) {
		t.Fatal("shutdown omitted best-effort event loss", logs.String())
	}
	receive(t, disconnected)
	assertListenerClosed(t, host)
}

type reportShutdownLog struct {
	slog.Handler
	written      chan struct{}
	release      chan struct{}
	shuttingDown chan struct{}
	requestDone  chan struct{}
}

func (h reportShutdownLog) Handle(ctx context.Context, record slog.Record) error {
	switch record.Message {
	case "report write":
		close(h.written)
		<-h.release
	case "server shutting down":
		close(h.shuttingDown)
	case "http request":
		defer close(h.requestDone)
	}
	return h.Handler.Handle(ctx, record)
}

func TestRunKeepsEventAdmissionOpenUntilHTTPShutdownCompletes(t *testing.T) {
	for _, mode := range []string{"graceful", "http-timeout"} {
		t.Run(mode, func(t *testing.T) {
			broker, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			settings := config.Defaults()
			settings.ListenAddress = "127.0.0.1:0"
			settings.DatabaseDSN = filepath.Join(t.TempDir(), "registry.db")
			settings.ReportRoot = t.TempDir()
			settings.ShutdownTimeout = 2 * time.Second
			if mode == "http-timeout" {
				settings.ShutdownTimeout = 50 * time.Millisecond
			}
			eventSettings := config.EventingDefaults()
			eventSettings.Endpoint, eventSettings.Address = "amqp://"+broker.Addr().String(), "opaque"
			eventSettings.ConnectTimeout, eventSettings.PublishTimeout = time.Hour, time.Hour
			eventSettings.ShutdownTimeout = 30 * time.Millisecond
			settings.Eventing = &eventSettings
			store, err := database.Connect(context.Background(), "sqlite", settings.DatabaseDSN, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer store.DB.Close()
			machine, err := store.CreateMachineWithKey(context.Background(), "in-flight-report")
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			gate := reportShutdownLog{Handler: slog.NewJSONHandler(&logs, nil),
				written: make(chan struct{}), release: make(chan struct{}),
				shuttingDown: make(chan struct{}), requestDone: make(chan struct{})}
			release := sync.OnceFunc(func() { close(gate.release) })
			defer release()
			address := make(chan string, 1)
			logger := slog.New(listeningLog{Handler: gate, address: address})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, settings, logger) }()
			host := receive(t, address)
			client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			body := `{"configurationName":"inventory",
				"configurationRevision":"550e8400-e29b-41d4-a716-446655440000",
				"uploadId":"550e8400-e29b-41d4-a716-446655440001",
				"result":{"schemaVersion":1,"configuration":"inventory.json","operation":"test",
				"startedAt":"2026-10-18T12:00:00Z","finishedAt":"2026-10-18T12:00:00.005Z",
				"durationMs":5,"outcome":"succeeded","exitCode":null,"dscResult":{},"error":null,"stderr":""}}`
			request, err := http.NewRequest(http.MethodPost, "http://"+host+"/v1/agent/reports", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "ApiKey "+machine.APIKey)
			request.Header.Set("Content-Type", "application/json")
			type requestResult struct {
				status int
				err    error
			}
			result := make(chan requestResult, 1)
			go func() {
				response, err := client.Do(request)
				status := 0
				if err == nil {
					status = response.StatusCode
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				result <- requestResult{status, err}
			}()
			select {
			case <-gate.written:
			case response := <-result:
				t.Fatal("report failed before the persistence barrier", response)
			case <-time.After(5 * time.Second):
				t.Fatal("report did not reach the persistence barrier")
			}
			cancel()
			receive(t, gate.shuttingDown)
			if mode == "http-timeout" {
				if err := receive(t, done); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("eventing masked the HTTP shutdown failure", err)
				}
				release()
				receive(t, result)
			} else {
				timer := time.NewTimer(3 * eventSettings.ShutdownTimeout)
				defer timer.Stop()
				select {
				case err := <-done:
					t.Fatal("event drain started before the active report finished", err)
				case <-timer.C:
				}
				release()
				if response := receive(t, result); response.err != nil || response.status != http.StatusCreated {
					t.Fatal("in-flight report did not finish during graceful shutdown", response)
				}
				if err := receive(t, done); err != nil {
					t.Fatal("best-effort event drain failed clean HTTP shutdown", err)
				}
			}
			receive(t, gate.requestDone)
			reports, err := store.ListReports(context.Background(), machine.ID, "", 10)
			want := 1
			if mode == "http-timeout" {
				want = 0
			}
			if err != nil || len(reports) != want {
				t.Fatal("incorrect report persistence across shutdown", reports, err)
			}
			queued := strings.Contains(logs.String(), `"outcome":"queued","event_type":"report.created"`)
			if queued != (mode == "graceful") {
				t.Fatal("late committed report event was lost or a failed report emitted one", logs.String())
			}
			assertListenerClosed(t, host)
		})
	}
}

func TestEventingInvalidCredentialsFailBeforeListening(t *testing.T) {
	settings := config.Defaults()
	settings.DatabaseDSN = filepath.Join(t.TempDir(), "registry.db")
	settings.ReportRoot = t.TempDir()
	eventSettings := config.EventingDefaults()
	eventSettings.Endpoint, eventSettings.Address = "amqps://broker.example", "opaque"
	eventSettings.Auth = config.EventingAuth{Type: "plain", Username: "registry", PasswordEnv: "DSCREGISTRY_UNIT_SERVER_PASSWORD"}
	settings.Eventing = &eventSettings
	t.Setenv(eventSettings.Auth.PasswordEnv, "")
	if err := run(context.Background(), settings, testLogger()); err == nil {
		t.Fatal("invalid local credentials were silently ignored")
	}
	// Bootstrap uses only database settings, without resolving event secrets.
	body, err := json.Marshal(map[string]any{
		"database": map[string]string{"engine": "sqlite", "dsn": settings.DatabaseDSN},
		"eventing": map[string]any{"endpoint": eventSettings.Endpoint, "address": "opaque",
			"auth": map[string]string{"type": "plain", "username": "registry", "passwordEnv": eventSettings.Auth.PasswordEnv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap(context.Background(), []string{"-config", path}, io.Discard); err != nil {
		t.Fatal("bootstrap required eventing credentials", err)
	}
}
