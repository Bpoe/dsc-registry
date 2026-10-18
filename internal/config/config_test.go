package config

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	s, err := Parse([]string{"-development-http"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.DevelopmentHTTP = true
	if s != want {
		t.Fatalf("settings = %+v; want %+v", s, want)
	}
	s, err = Parse([]string{
		"-external-https", "-listen", "[::]:8443", "-database-dsn", "other.db",
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

func TestInvalidSettings(t *testing.T) {
	tests := map[string]func(*Settings){
		"implicit transport":      func(s *Settings) { s.DevelopmentHTTP = false },
		"both transports":         func(s *Settings) { s.ExternalHTTPS = true },
		"public development":      func(s *Settings) { s.ListenAddress = "0.0.0.0:8080" },
		"hostname":                func(s *Settings) { s.ListenAddress = "localhost:8080" },
		"empty host":              func(s *Settings) { s.ListenAddress = ":8080" },
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
			s.DevelopmentHTTP = true
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
}

func TestFlagErrorsAndHelp(t *testing.T) {
	for _, args := range [][]string{
		nil, {"-unknown"}, {"-development-http", "extra"},
		{"-development-http", "-probe-timeout", "invalid"}, {"-max-request-bytes", "oops"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid args %q", args)
		}

	}
	var help bytes.Buffer
	if _, err := Parse([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) || help.Len() == 0 {
		t.Fatalf("help: error=%v, output=%q", err, help.String())
	}
	if _, err := Parse([]string{"-development-http", "-listen", "[::1]:0"}, io.Discard); err != nil {
		t.Fatalf("IPv6 loopback: %v", err)
	}
}

func TestPostgresSettingsAndBootstrap(t *testing.T) {
	settings, err := Parse([]string{"-external-https", "-database-engine", "postgres",
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
