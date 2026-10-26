package config

import (
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEventingFileDefaultsAndRelativePaths(t *testing.T) {
	path := writeConfig(t, `{"eventing":{"endpoint":"amqps://broker.example","address":"/queues/events",
		"auth":{"type":"external"},"tls":{"caFile":"certs/ca.pem","certFile":"certs/client.pem","keyFile":"certs/key.pem"},
		"future":{"anything":[null,1e999]}}}`)
	got, err := Parse([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := EventingDefaults()
	want.Endpoint, want.Address, want.Auth.Type = "amqps://broker.example", "/queues/events", "external"
	want.TLS = EventingTLS{
		CAFile:   filepath.Join(filepath.Dir(path), "certs", "ca.pem"),
		CertFile: filepath.Join(filepath.Dir(path), "certs", "client.pem"),
		KeyFile:  filepath.Join(filepath.Dir(path), "certs", "key.pem"),
	}
	if !reflect.DeepEqual(got.Eventing, &want) {
		t.Fatalf("incorrect event defaults/paths: %+v", got.Eventing)
	}
	path = writeConfig(t, `{"eventing":{"type":"amqp1","endpoint":"amqp://broker:5672","address":"opaque-address",
		"auth":{"type":"plain","username":"registry","passwordEnv":"TEST_PASSWORD"},
		"queueCapacity":7,"connectTimeout":"1s","publishTimeout":"2s","shutdownTimeout":"3s"}}`)
	got, err = Parse([]string{"-config", path}, io.Discard)
	if err != nil || got.Eventing.QueueCapacity != 7 || got.Eventing.PublishTimeout != 2*time.Second ||
		got.Eventing.ConnectTimeout != time.Second || got.Eventing.ShutdownTimeout != 3*time.Second ||
		got.Eventing.Auth.PasswordEnv != "TEST_PASSWORD" {
		t.Fatal("incorrect event settings", got.Eventing, err)
	}
}

func TestEventingDisabledAndBootstrapIsolation(t *testing.T) {
	for _, body := range []string{`{}`, `{"eventing":{"futureFeature":true}}`} {
		got, err := Parse([]string{"-config", writeConfig(t, body)}, io.Discard)
		if err != nil || got.Eventing != nil {
			t.Fatal("omitted/unknown eventing enabled", err)
		}
	}
	_, _, err := ParseBootstrap([]string{"-config", writeConfig(t, `{"eventing":{"type":"future","endpoint":"invalid"}}`)}, io.Discard)
	if err != nil {
		t.Fatal("bootstrap validated server-only event settings", err)
	}
}

func TestInvalidEventingSettingsAreRedacted(t *testing.T) {
	for _, change := range []func(*EventingSettings){
		func(s *EventingSettings) { s.Type = "private-secret" },
		func(s *EventingSettings) { s.Endpoint = "http://private-secret" },
		func(s *EventingSettings) { s.Endpoint = "amqp://user:private-secret@broker" },
		func(s *EventingSettings) { s.Endpoint = "amqp://broker/private-secret" },
		func(s *EventingSettings) { s.Endpoint = "amqp://broker?secret=private-secret" },
		func(s *EventingSettings) { s.Endpoint = "amqp://broker:999999" },
		func(s *EventingSettings) { s.Endpoint = "amqp://broker:" },
		func(s *EventingSettings) { s.Endpoint = "amqp://:5672" },
		func(s *EventingSettings) { s.Address = " " },
		func(s *EventingSettings) { s.QueueCapacity = 0 },
		func(s *EventingSettings) { s.QueueCapacity = 1000001 },
		func(s *EventingSettings) { s.ConnectTimeout = 0 },
		func(s *EventingSettings) { s.PublishTimeout = -time.Second },
		func(s *EventingSettings) { s.ShutdownTimeout = 0 },
		func(s *EventingSettings) { s.Auth.Type = "private-secret" },
		func(s *EventingSettings) { s.Auth.Type = "plain" },
		func(s *EventingSettings) { s.Auth.PasswordEnv = "private-secret" },
		func(s *EventingSettings) { s.Auth.Type = "external" },
		func(s *EventingSettings) { s.TLS.CAFile = "private-secret" },
		func(s *EventingSettings) { s.Endpoint = "amqps://broker"; s.TLS.CertFile = "private-secret" },
	} {
		settings := EventingDefaults()
		settings.Endpoint, settings.Address = "amqp://broker", "/queues/events"
		change(&settings)
		err := settings.Validate()
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid setting accepted or leaked", err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"queueCapacity":null}`, `{"queueCapacity":1.5}`,
		`{"publishTimeout":4}`, `{"auth":null}`, `{"auth":{"type":"plain","type":"plain"}}`,
		`{"tls":{"caFile":null}}`, `{"future":{"private-secret":1,"private-secret":2}}`,
	} {
		_, err := Parse([]string{"-config", writeConfig(t, `{"eventing":`+raw+`}`)}, io.Discard)
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid eventing file accepted or disclosed", raw, err)
		}
	}
}
