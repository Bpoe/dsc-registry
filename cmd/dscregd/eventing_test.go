package main

import (
	"context"
	"encoding/json"
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
	logger := slog.New(listeningLog{Handler: slog.NewJSONHandler(io.Discard, nil), address: address})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, settings, logger) }()
	host := receive(t, address)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "http://"+host+"/v1/machines", strings.NewReader(`{"name":"event-host"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "ApiKey "+admin.Key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("HTTP waited for broker communication", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatal("broker outage changed mutation response", response.StatusCode)
	}
	receive(t, connected)
	response, err = client.Get("http://" + host + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("broker outage changed readiness")
	}
	cancel()
	if err := receive(t, done); err == nil {
		t.Fatal("expected event drain deadline while broker is unavailable")
	}
	receive(t, disconnected)
	assertListenerClosed(t, host)
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
