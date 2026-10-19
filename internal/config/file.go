package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func mergeFile(flags *flag.FlagSet, path string, s *Settings) error {
	overrides := make(map[string]string)
	flags.Visit(func(f *flag.Flag) { overrides[f.Name] = f.Value.String() })
	if _, specified := overrides["config"]; !specified {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read configuration file")
	}
	if !utf8.Valid(data) {
		return errors.New("configuration file must be UTF-8 JSON")
	}
	// Flag values are saved before loading so even explicit default values win.
	var tls TLSSettings
	fields := map[string]any{
		"listenAddress": &s.ListenAddress, "externalHttps": &s.ExternalHTTPS,
		"tls":             map[string]any{"certFile": &tls.CertFile, "keyFile": &tls.KeyFile},
		"developmentHttp": &s.DevelopmentHTTP, "reportRoot": &s.ReportRoot,
		"pollMaxAge": &s.PollMaxAge, "maxRequestBytes": &s.MaxRequestBytes,
		"maxReportBytes": &s.MaxReportBytes, "readHeaderTimeout": &s.ReadHeaderTimeout,
		"readTimeout": &s.ReadTimeout, "writeTimeout": &s.WriteTimeout,
		"idleTimeout": &s.IdleTimeout, "startupTimeout": &s.StartupTimeout,
		"shutdownTimeout": &s.ShutdownTimeout, "probeTimeout": &s.ProbeTimeout,
		"sqliteBusyTimeout": &s.SQLiteBusyTimeout,
		"database":          map[string]any{"engine": &s.DatabaseEngine, "dsn": &s.DatabaseDSN},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	present := make(map[string]bool)
	if err := decodeObject(decoder, fields, "", present); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("configuration file must contain a single JSON object")
	}
	if present["tls"] {
		s.TLS = &tls
	}
	for name, value := range overrides {
		if err := flags.Set(name, value); err != nil {
			return errors.New("cannot apply command-line override")
		}
	}
	if err := s.validateDatabase(); err != nil {
		return err
	}
	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return errors.New("cannot resolve configuration file directory")
	}
	if present["reportRoot"] && !hasOverride(overrides, "report-root") &&
		strings.TrimSpace(s.ReportRoot) != "" && !filepath.IsAbs(s.ReportRoot) {
		s.ReportRoot = filepath.Join(directory, s.ReportRoot)
	}
	if present["database.dsn"] && !hasOverride(overrides, "database-dsn") &&
		s.DatabaseEngine == "sqlite" && !filepath.IsAbs(s.DatabaseDSN) {
		s.DatabaseDSN = filepath.Join(directory, s.DatabaseDSN)
	}
	if s.TLS != nil {
		for _, path := range []*string{&s.TLS.CertFile, &s.TLS.KeyFile} {
			if strings.TrimSpace(*path) != "" && !filepath.IsAbs(*path) {
				*path = filepath.Join(directory, *path)
			}
		}
	}
	return nil
}

func hasOverride(overrides map[string]string, name string) bool {
	_, ok := overrides[name]
	return ok
}

// Decode fields individually to reject duplicate keys and null scalars, which
// encoding/json otherwise accepts. Never include decoder errors or input in errors.
func decodeObject(decoder *json.Decoder, fields map[string]any, prefix string, present map[string]bool) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("configuration file requires JSON objects")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("malformed configuration JSON")
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("malformed configuration JSON")
		}
		target, known := fields[name]
		if !known {
			return errors.New("unknown configuration property")
		}
		setting := prefix + name
		if seen[name] {
			return fmt.Errorf("duplicate configuration property %s", setting)
		}
		seen[name], present[setting] = true, true
		if nested, ok := target.(map[string]any); ok {
			if err := decodeObject(decoder, nested, setting+".", present); err != nil {
				return err
			}
			continue
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("invalid configuration property %s", setting)
		}
		if bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("configuration property %s must not be null", setting)
		}
		if duration, ok := target.(*time.Duration); ok {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return fmt.Errorf("configuration property %s requires a duration string", setting)
			}
			value, err := time.ParseDuration(text)
			if err != nil {
				return fmt.Errorf("invalid duration for configuration property %s", setting)
			}
			*duration = value
		} else if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("invalid type for configuration property %s", setting)
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("malformed configuration JSON")
	}
	return nil
}
