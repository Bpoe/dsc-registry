package config

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	s, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	if s != want {
		t.Fatalf("settings = %+v; want %+v", s, want)
	}
	if s.ListenAddress != "127.0.0.1:8080" || s.TLS != nil {
		t.Fatal("defaults must select loopback HTTP")
	}
	s, err = Parse([]string{
		"-listen", "[::]:8443", "-database-dsn", "other.db",
		"-report-root", "private-reports", "-poll-max-age", "60s",
		"-max-request-bytes", "2048", "-max-report-bytes", "4096",
		"-read-header-timeout", "1s", "-read-timeout", "2s", "-write-timeout", "3s",
		"-idle-timeout", "4s", "-startup-timeout", "5s", "-shutdown-timeout", "6s",
		"-probe-timeout", "500ms", "-sqlite-busy-timeout", "100ms",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.ListenAddress != "[::]:8443" || s.DatabaseDSN != "other.db" ||
		s.ReportRoot != "private-reports" || s.PollMaxAge != time.Minute ||
		s.MaxRequestBytes != 2048 || s.MaxReportBytes != 4096 ||
		s.ReadHeaderTimeout != time.Second || s.ReadTimeout != 2*time.Second ||
		s.WriteTimeout != 3*time.Second || s.IdleTimeout != 4*time.Second ||
		s.StartupTimeout != 5*time.Second || s.ShutdownTimeout != 6*time.Second ||
		s.ProbeTimeout != 500*time.Millisecond || s.SQLiteBusyTimeout != 100*time.Millisecond {
		t.Fatalf("flags were not applied: %+v", s)
	}
}

func TestHTTPListenAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:8080", "0.0.0.0:8080", "192.168.1.10:8080",
		"localhost:8080", "registry.example.com:8080", ":8080",
		"[::]:8080", "[::1]:0", "[fe80::1%eth0]:8080",
	} {
		t.Run(address, func(t *testing.T) {
			settings, err := Parse([]string{"-listen", address}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if settings.ListenAddress != address || settings.TLS != nil {
				t.Fatalf("HTTP listen address not preserved: %+v", settings)
			}
		})
	}
}

func TestInvalidSettings(t *testing.T) {
	tests := map[string]func(*Settings){
		"empty listen address":    func(s *Settings) { s.ListenAddress = "" },
		"missing port":            func(s *Settings) { s.ListenAddress = "127.0.0.1" },
		"unbracketed IPv6":        func(s *Settings) { s.ListenAddress = "::1:8080" },
		"empty port":              func(s *Settings) { s.ListenAddress = "127.0.0.1:" },
		"invalid port":            func(s *Settings) { s.ListenAddress = "127.0.0.1:65536" },
		"negative port":           func(s *Settings) { s.ListenAddress = "127.0.0.1:-1" },
		"service port":            func(s *Settings) { s.ListenAddress = "127.0.0.1:http" },
		"engine":                  func(s *Settings) { s.DatabaseEngine = "mysql" },
		"empty dsn":               func(s *Settings) { s.DatabaseDSN = " " },
		"memory":                  func(s *Settings) { s.DatabaseDSN = ":memory:" },
		"uri":                     func(s *Settings) { s.DatabaseDSN = "file:test.db?mode=memory" },
		"network database":        func(s *Settings) { s.DatabaseDSN = `\\server\share\registry.db` },
		"empty report root":       func(s *Settings) { s.ReportRoot = "" },
		"database as report root": func(s *Settings) { s.ReportRoot = s.DatabaseDSN },
		"poll age":                func(s *Settings) { s.PollMaxAge = 0 },
		"fractional poll seconds": func(s *Settings) { s.PollMaxAge = time.Millisecond },
		"request limit":           func(s *Settings) { s.MaxRequestBytes = 0 },
		"report limit":            func(s *Settings) { s.MaxReportBytes = 0 },
		"read header timeout":     func(s *Settings) { s.ReadHeaderTimeout = 0 },
		"read timeout":            func(s *Settings) { s.ReadTimeout = -1 },
		"write timeout":           func(s *Settings) { s.WriteTimeout = 0 },
		"idle timeout":            func(s *Settings) { s.IdleTimeout = 0 },
		"startup timeout":         func(s *Settings) { s.StartupTimeout = 0 },
		"shutdown timeout":        func(s *Settings) { s.ShutdownTimeout = 0 },
		"probe timeout":           func(s *Settings) { s.ProbeTimeout = 0 },
		"header after read":       func(s *Settings) { s.ReadHeaderTimeout = s.ReadTimeout + 1 },
		"probe after write":       func(s *Settings) { s.ProbeTimeout = s.WriteTimeout },
		"busy timeout":            func(s *Settings) { s.SQLiteBusyTimeout = 0 },
		"excessive busy timeout":  func(s *Settings) { s.SQLiteBusyTimeout = 2 * time.Minute },
		"fractional milliseconds": func(s *Settings) { s.SQLiteBusyTimeout = time.Nanosecond },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := Defaults()
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
}

func TestFlagErrorsAndHelp(t *testing.T) {
	for _, args := range [][]string{
		{"-unknown"}, {"extra"}, {"-development-http"}, {"-external-https"},
		{"-development-http=false"}, {"-external-https=false"},
		{"-probe-timeout", "invalid"}, {"-max-request-bytes", "oops"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid args %q", args)
		}

	}
	var help bytes.Buffer
	if _, err := Parse([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) || help.Len() == 0 {
		t.Fatalf("help: error=%v, output=%q", err, help.String())
	}
	for _, removed := range []string{"development-http", "external-https"} {
		if strings.Contains(help.String(), removed) {
			t.Fatalf("help documents removed flag %q", removed)
		}
	}
}

func TestPostgresSettingsAndBootstrap(t *testing.T) {
	settings, err := Parse([]string{"-database-engine", "postgres",
		"-database-dsn", "postgres://operator@localhost/registry?sslmode=verify-full"}, io.Discard)
	if err != nil || settings.DatabaseEngine != "postgres" {
		t.Fatal("PostgreSQL settings rejected")
	}
	parsed, name, err := ParseBootstrap([]string{"-database-engine", "postgres",
		"-database-dsn", "host=localhost dbname=registry", "-name", "operator"}, io.Discard)
	if err != nil || parsed.DatabaseEngine != "postgres" || name != "operator" {
		t.Fatalf("bootstrap settings: %v", err)
	}
	for _, args := range [][]string{{"-name", ""}, {"-name", "bad\nname"}, {"extra"}, {"-unknown"}, {"-startup-timeout", "0s"}} {
		if _, _, err := ParseBootstrap(args, io.Discard); err == nil {
			t.Fatalf("invalid bootstrap arguments accepted: %v", args)
		}
	}
	var help bytes.Buffer
	if _, _, err := ParseBootstrap([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) || help.Len() == 0 {
		t.Fatal("bootstrap help unavailable")
	}
}
