package eventing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/google/uuid"
)

func rabbitSettings(t *testing.T) (config.EventingSettings, string) {
	t.Helper()
	endpoint := os.Getenv("DSCREGISTRY_TEST_AMQP_ENDPOINT")
	if endpoint == "" {
		t.Skip("RabbitMQ 4.x integration requires DSCREGISTRY_TEST_AMQP_ENDPOINT and management/credential settings; see docs/eventing.md")
	}
	management := os.Getenv("DSCREGISTRY_TEST_AMQP_MANAGEMENT_URL")
	username := os.Getenv("DSCREGISTRY_TEST_AMQP_USERNAME")
	if management == "" || username == "" || os.Getenv("DSCREGISTRY_TEST_AMQP_PASSWORD") == "" {
		t.Fatal("RabbitMQ integration requires management URL, username and password")
	}
	settings := amqpSettings()
	settings.Endpoint = endpoint
	settings.Auth = config.EventingAuth{Type: "plain", Username: username, PasswordEnv: "DSCREGISTRY_TEST_AMQP_PASSWORD"}
	name := "dscreg-test-" + uuid.NewString()
	settings.Address = "/queues/" + name
	queueURL := strings.TrimRight(management, "/") + "/api/queues/%2F/" + url.PathEscape(name)
	rabbitHTTP(t, settings, http.MethodPut, queueURL, `{"durable":false,"auto_delete":false,"arguments":{}}`)
	t.Cleanup(func() { rabbitHTTP(t, settings, http.MethodDelete, queueURL, "") })
	return settings, queueURL
}

func rabbitHTTP(t *testing.T, settings config.EventingSettings, method, endpoint, body string) {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal("invalid RabbitMQ management URL")
	}
	request.SetBasicAuth(settings.Auth.Username, os.Getenv(settings.Auth.PasswordEnv))
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("RabbitMQ management request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("RabbitMQ provisioning returned HTTP %d", response.StatusCode)
	}
}

func TestRabbitMQPublishReuseFailureRecoveryAndInvalidDestination(t *testing.T) {
	settings, _ := rabbitSettings(t)
	var logs bytes.Buffer
	publisher, err := NewAMQP(settings, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := newEvent(MachineCreated, Identifiers{MachineID: uuid.NewString()})
	second := newEvent(ReportUpdated, Identifiers{MachineID: first.Data.MachineID, ConfigurationName: "inventory", ReportID: uuid.NewString()})
	if err := publisher.Publish(ctx, first); err != nil {
		t.Fatal("initial publish failed", err)
	}
	connection := publisher.conn
	if err := publisher.Publish(ctx, second); err != nil || publisher.conn != connection {
		t.Fatal("connection not reused", err)
	}
	// Break a persistent socket, observe a failed attempt, then recover on
	// the next event after bounded backoff. Failed events are not retried.
	if err := publisher.network.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, first); err == nil {
		t.Fatal("broken connection was reported as successful")
	}
	third := newEvent(MachineDeleted, first.Data)
	if err := publisher.Publish(ctx, third); err != nil || publisher.conn == connection {
		t.Fatal("publisher did not reconnect", err)
	}
	if !strings.Contains(logs.String(), `"outcome":"reconnected"`) {
		t.Fatal("missing reconnect signal")
	}
	options, err := connectionOptions(settings)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := amqp.Dial(ctx, settings.Endpoint, &options)
	if err != nil {
		t.Fatal("receiver connection failed")
	}
	defer consumer.Close()
	session, err := consumer.NewSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := session.NewReceiver(ctx, settings.Address, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Event{first, second, third} {
		message, err := receiver.Receive(ctx, nil)
		if err != nil {
			t.Fatal("could not receive published event", err)
		}
		var got Event
		if err := json.Unmarshal(message.GetData(), &got); err != nil || got != want ||
			message.Properties.ContentType == nil || *message.Properties.ContentType != contentType {
			t.Fatal("broker changed event or content type", err)
		}
		if err := receiver.AcceptMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	publisher.settings.Address = "/queues/nonexistent-" + uuid.NewString()
	if err := publisher.Publish(ctx, first); err == nil {
		t.Fatal("nonexistent destination accepted")
	}
	publisher.settings.Address = settings.Address
	if err := publisher.Publish(ctx, newEvent(MachineCreated, first.Data)); err != nil {
		t.Fatal("invalid destination permanently stopped publishing", err)
	}
}

func TestRabbitMQPublishDeadline(t *testing.T) {
	settings, queueURL := rabbitSettings(t)
	// A max-length zero queue with reject-publish explicitly rejects delivery.
	rabbitHTTP(t, settings, http.MethodDelete, queueURL, "")
	rabbitHTTP(t, settings, http.MethodPut, queueURL,
		`{"durable":false,"arguments":{"x-max-length":0,"x-overflow":"reject-publish"}}`)
	publisher, err := NewAMQP(settings, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := publisher.Publish(ctx, newEvent(MachineCreated, Identifiers{})); err == nil {
		t.Fatal("rejected delivery was reported as successful")
	}
	// A canceled publish budget also interrupts reconnect backoff.
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if err := publisher.Publish(expired, Event{}); err == nil {
		t.Fatal("expired publish deadline ignored")
	}
}

func TestRabbitMQInFlightPublishTimeout(t *testing.T) {
	settings, _ := rabbitSettings(t)
	endpoint, err := url.Parse(settings.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Scheme != "amqp" {
		t.Skip("wire-stall fixture requires the plaintext disposable RabbitMQ listener")
	}
	port := endpoint.Port()
	if port == "" {
		port = "5672"
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var suppress atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		client, err := proxy.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		broker, err := net.DialTimeout("tcp", net.JoinHostPort(endpoint.Hostname(), port), time.Second)
		if err != nil {
			return
		}
		defer broker.Close()
		upstreamDone := make(chan struct{})
		go func() {
			defer close(upstreamDone)
			buffer := make([]byte, 65536)
			for {
				n, err := broker.Read(buffer)
				if n > 0 && !suppress.Load() {
					if _, err := client.Write(buffer[:n]); err != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		_, _ = io.Copy(broker, client)
		_ = broker.Close()
		_ = client.Close()
		<-upstreamDone
	}()
	settings.Endpoint = "amqp://" + proxy.Addr().String()
	publisher, err := NewAMQP(settings, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publisher.Close(); waitFor(t, done) }()
	connect, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := publisher.connect(connect); err != nil {
		t.Fatal("could not establish proxied AMQP connection", err)
	}
	// The peer stays connected, but no flow/disposition can reach the sender.
	suppress.Store(true)
	attempt, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := publisher.Publish(attempt, newEvent(MachineCreated, Identifiers{})); err == nil ||
		attempt.Err() != context.DeadlineExceeded || publisher.network != nil {
		t.Fatal("in-flight publish ignored its deadline or retained the stalled connection", err)
	}
}
