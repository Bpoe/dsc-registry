package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompleteFile(t *testing.T) {
	path := writeConfig(t, `{
		"listenAddress": "[::]:8443",
		"externalHttps": true,
		"developmentHttp": false,
		"database": {"engine": "postgres", "futureSetting": true, "dsn": "postgres://registry@localhost/registry?sslmode=verify-full"},
		"futureFeature": {"enabled": true, "options": [1, 2, 3]},
		"deprecatedSetting": null,
		"reportRoot": "private-reports",
		"pollMaxAge": "1m",
		"maxRequestBytes": 2048,
		"maxReportBytes": 4096,
		"readHeaderTimeout": "1s",
		"readTimeout": "2s",
		"writeTimeout": "3s",
		"idleTimeout": "4s",
		"startupTimeout": "5s",
		"shutdownTimeout": "6s",
		"probeTimeout": "500ms",
		"sqliteBusyTimeout": "100ms"
	}`)
	got, err := Parse([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		ListenAddress: "[::]:8443", ExternalHTTPS: true, DatabaseEngine: "postgres",
		DatabaseDSN: "postgres://registry@localhost/registry?sslmode=verify-full",
		ReportRoot:  filepath.Join(filepath.Dir(path), "private-reports"),
		PollMaxAge:  time.Minute, MaxRequestBytes: 2048, MaxReportBytes: 4096,
		ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: 4 * time.Second,
		StartupTimeout: 5 * time.Second, ShutdownTimeout: 6 * time.Second,
		ProbeTimeout: 500 * time.Millisecond, SQLiteBusyTimeout: 100 * time.Millisecond,
	}
	if got != want {
		t.Fatalf("settings = %+v; want %+v", got, want)
	}
}

func TestPartialFileDefaults(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"database":{}}`, `{"developmentHttp":true}`,
		`{"futureSetting":null}`, `{"database":{"futureSetting":true}}`,
		`{"DevelopmentHttp":false}`, `{"tls":{"CertFile":"ignored"}}`,
	} {
		path := writeConfig(t, body)
		got, err := Parse([]string{"-config", path, "-development-http"}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := Defaults()
		want.DevelopmentHTTP = true
		if got != want {
			t.Fatalf("partial file changed omitted defaults: %+v", got)
		}
	}
}

func TestFileUnknownProperties(t *testing.T) {
	values := map[string]string{
		"string": `"ignored"`, "integer": `42`, "number": `-1.25e3`,
		"large number": `1e1000`, "true": `true`, "false": `false`, "null": `null`,
		"empty object": `{}`, "empty array": `[]`,
		"object":              `{"enabled":true,"options":[1,{"nested":[false,null,"text"]},3]}`,
		"array":               `[{"nested":{"value":null}},[1,2,3],false,"text"]`,
		"known names":         `{"developmentHttp":null,"database":{"dsn":[]},"tls":{"certFile":false}}`,
		"scoped names":        `[{"value":1},{"value":2,"nested":{"value":3}}]`,
		"nested large number": `{"options":[1e1000,{"value":-1e1000}]}`,
	}
	scopes := map[string]string{
		"top level":        `"futureSetting":%s,"developmentHttp":true`,
		"database":         `"database":{"futureSetting":%s,"engine":"sqlite","dsn":"data/registry.db"},"developmentHttp":true`,
		"tls":              `"tls":{"futureSetting":%s,"certFile":"certs/server.crt","keyFile":"certs/server.key"}`,
		"tls only unknown": `"tls":{"futureSetting":%s},"developmentHttp":true`,
	}
	parsers := map[string]func(string) (Settings, error){
		"server": func(path string) (Settings, error) {
			return Parse([]string{"-config", path}, io.Discard)
		},
		"bootstrap": func(path string) (Settings, error) {
			settings, _, err := ParseBootstrap([]string{"-config", path}, io.Discard)
			return settings, err
		},
	}
	for scope, properties := range scopes {
		for name, value := range values {
			for parserName, parse := range parsers {
				t.Run(scope+"/"+name+"/"+parserName, func(t *testing.T) {
					body := "{" + fmt.Sprintf(properties, value) +
						`,"pollMaxAge":"90s","reportRoot":"data/reports"}`
					path := writeConfig(t, body)
					got, err := parse(path)
					if err != nil {
						t.Fatal(err)
					}
					want := Defaults()
					want.DevelopmentHTTP = scope != "tls"
					want.PollMaxAge = 90 * time.Second
					want.ReportRoot = filepath.Join(filepath.Dir(path), "data", "reports")
					if scope == "database" {
						want.DatabaseDSN = filepath.Join(filepath.Dir(path), "data", "registry.db")
					}
					if scope == "tls" {
						want.TLS = &TLSSettings{
							CertFile: filepath.Join(filepath.Dir(path), "certs", "server.crt"),
							KeyFile:  filepath.Join(filepath.Dir(path), "certs", "server.key"),
						}
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("settings = %+v; want %+v", got, want)
					}
				})
			}
		}
	}
}

func TestFileNestedUnknownDuplicates(t *testing.T) {
	values := map[string]string{
		"object":          `{"password-secret":1,"password-secret":2}`,
		"nested object":   `{"options":{"password-secret":null,"password-secret":[]}}`,
		"array":           `[{"password-secret":true,"password-secret":false}]`,
		"nested arrays":   `[null,[[{"password-secret":{},"password-secret":"secret"}]]]`,
		"object in array": `{"options":[1,{"password-secret":1,"password-secret":2}]}`,
		"escaped name":    `{"password-secret":1,"password\u002dsecret":2}`,
	}
	for scope, properties := range map[string]string{
		"top level": `"futureSetting":%s`,
		"database":  `"database":{"futureSetting":%s}`,
		"tls":       `"tls":{"futureSetting":%s}`,
	} {
		for name, value := range values {
			t.Run(scope+"/"+name, func(t *testing.T) {
				path := writeConfig(t, "{"+fmt.Sprintf(properties, value)+"}")
				_, serverErr := Parse([]string{"-config", path, "-development-http"}, io.Discard)
				_, _, bootstrapErr := ParseBootstrap([]string{"-config", path}, io.Discard)
				for _, err := range []error{serverErr, bootstrapErr} {
					if err == nil || !strings.Contains(err.Error(), "duplicate configuration property") {
						t.Fatalf("expected duplicate-property error, got %v", err)
					}
					if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), path) {
						t.Fatal("configuration error disclosed input")
					}
				}
			})
		}
	}
}

func TestFileCLIOverrides(t *testing.T) {
	path := writeConfig(t, `{
		"listenAddress":"127.0.0.1:9090", "externalHttps":true,
		"developmentHttp":false, "pollMaxAge":"1m",
		"database":{"engine":"postgres","futureSetting":[1,null,{}],"dsn":"secret-dsn"},
		"futureFeature":{"enabled":true}, "tls":{"futureSetting":true},
		"reportRoot":"from-file", "maxRequestBytes":4096
	}`)
	got, err := Parse([]string{
		"-listen", Defaults().ListenAddress, "-config", path,
		"-external-https=false", "-development-http=true",
		"-database-engine", "sqlite", "-database-dsn", "registry.db",
		"-report-root", "reports", "-max-request-bytes", "1048576",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.DevelopmentHTTP = true
	want.PollMaxAge = time.Minute
	if got != want {
		t.Fatalf("explicit default-valued overrides not preserved: %+v", got)
	}
	path = writeConfig(t, `{"developmentHttp":true}`)
	got, err = Parse([]string{"-config", path, "-development-http=false", "-external-https"}, io.Discard)
	if err != nil || got.DevelopmentHTTP || !got.ExternalHTTPS {
		t.Fatal("explicit boolean false did not override file")
	}
}

func TestFileErrors(t *testing.T) {
	tests := map[string]string{
		"malformed":                   `{"developmentHttp":true`,
		"trailing comma":              `{"developmentHttp":true,}`,
		"trailing garbage":            `{"developmentHttp":true}secret`,
		"multiple documents":          `{"developmentHttp":true}{}`,
		"duplicate":                   `{"developmentHttp":true,"developmentHttp":false}`,
		"escaped duplicate":           `{"developmentHttp":true,"development\u0048ttp":false}`,
		"nested duplicate":            `{"database":{"engine":"sqlite","engine":"postgres"}}`,
		"unknown duplicate":           `{"password-secret":1,"password-secret":2}`,
		"escaped unknown duplicate":   `{"futureSetting":1,"future\u0053etting":2}`,
		"nested unknown duplicate":    `{"database":{"password-secret":null,"password-secret":[]}}`,
		"TLS unknown duplicate":       `{"tls":{"password-secret":true,"password-secret":false}}`,
		"duplicate after unknown":     `{"developmentHttp":true,"futureSetting":{},"developmentHttp":false}`,
		"unknown missing value":       `{"futureSetting":}`,
		"unknown malformed object":    `{"futureSetting":{"enabled":true,}}`,
		"unknown malformed array":     `{"futureSetting":[1,2,]}`,
		"unknown incomplete object":   `{"futureSetting":{"enabled":true}`,
		"unknown incomplete array":    `{"futureSetting":[1,2}`,
		"unknown invalid literal":     `{"futureSetting":undefined}`,
		"unknown invalid number":      `{"futureSetting":01}`,
		"unknown trailing garbage":    `{"futureSetting":null}secret`,
		"nested malformed unknown":    `{"database":{"futureSetting":[{} true]}}`,
		"TLS malformed unknown":       `{"tls":{"futureSetting":{"enabled":}}}`,
		"string boolean":              `{"developmentHttp":"secret"}`,
		"number string":               `{"listenAddress":123}`,
		"fractional bytes":            `{"maxRequestBytes":1.5}`,
		"overflow bytes":              `{"maxRequestBytes":9223372036854775808}`,
		"string bytes":                `{"maxReportBytes":"secret"}`,
		"null":                        `null`,
		"array":                       `[]`,
		"null boolean":                `{"developmentHttp":null}`,
		"null database":               `{"database":null}`,
		"array database":              `{"database":[]}`,
		"null dsn":                    `{"database":{"dsn":null}}`,
		"number engine":               `{"database":{"engine":3}}`,
		"null duration":               `{"pollMaxAge":null}`,
		"number duration":             `{"pollMaxAge":300000000000}`,
		"invalid duration":            `{"pollMaxAge":"secret"}`,
		"overflow duration":           `{"pollMaxAge":"999999999999h"}`,
		"invalid utf8":                "{\"listenAddress\":\"\xffsecret\"}",
		"nested invalid content":      `{"database":{"dsn":{"secret":"secret"}}}`,
		"duplicate TLS":               `{"tls":{},"tls":{}}`,
		"duplicate certificate":       `{"tls":{"certFile":"secret","certFile":"secret"}}`,
		"escaped duplicate TLS":       `{"tls":{"keyFile":"secret","key\u0046ile":"secret"}}`,
		"null TLS":                    `{"tls":null}`,
		"array TLS":                   `{"tls":[]}`,
		"null certificate":            `{"tls":{"certFile":null}}`,
		"number key":                  `{"tls":{"keyFile":123}}`,
		"invalid known after unknown": `{"futureSetting":[null,{}],"database":{"futureSetting":true,"dsn":null}}`,
		"invalid TLS after unknown":   `{"tls":{"futureSetting":null,"certFile":false}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, body)
			_, err := Parse([]string{"-config", path, "-development-http"}, io.Discard)
			if err == nil {
				t.Fatal("invalid file accepted")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), path) {
				t.Fatal("configuration error disclosed input")
			}
			if _, _, err := ParseBootstrap([]string{"-config", path}, io.Discard); err == nil {
				t.Fatal("bootstrap accepted invalid JSON or types")
			}
		})
	}
}

func TestUnreadableFile(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if _, err := Parse([]string{"-config", path}, io.Discard); err == nil {
			t.Fatal("unreadable configuration accepted")
		}
		if _, _, err := ParseBootstrap([]string{"-config", path}, io.Discard); err == nil {
			t.Fatal("bootstrap accepted unreadable configuration")
		}
	}
	t.Run("permissions", func(t *testing.T) {
		path := writeConfig(t, `{}`)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if _, err := os.ReadFile(path); err == nil {
			t.Skip("platform or current account permits reading mode-000 files")
		}
		if _, err := Parse([]string{"-config", path}, io.Discard); err == nil {
			t.Fatal("permission-denied configuration accepted")
		}
	})
}

func TestFileValidation(t *testing.T) {
	for _, body := range []string{
		`{"listenAddress":""}`,
		`{"database":{"engine":""}}`, `{"database":{"dsn":""}}`,
		`{"database":{"dsn":":memory:"}}`, `{"database":{"dsn":"file:registry.db"}}`,
		`{"database":{"dsn":"//server/share/registry.db"}}`,
		`{"reportRoot":""}`, `{"pollMaxAge":"0s"}`, `{"pollMaxAge":"500ms"}`,
		`{"maxRequestBytes":0}`, `{"maxReportBytes":0}`, `{"maxReportBytes":1}`,
		`{"readHeaderTimeout":"0s"}`, `{"readTimeout":"0s"}`,
		`{"writeTimeout":"0s"}`, `{"idleTimeout":"0s"}`,
		`{"startupTimeout":"0s"}`, `{"shutdownTimeout":"0s"}`,
		`{"probeTimeout":"0s"}`, `{"sqliteBusyTimeout":"0s"}`,
		`{"readHeaderTimeout":"31s"}`, `{"probeTimeout":"30s"}`,
		`{"sqliteBusyTimeout":"1ns"}`, `{"sqliteBusyTimeout":"61s"}`,
	} {
		t.Run(body, func(t *testing.T) {
			path := writeConfig(t, body)
			if _, err := Parse([]string{"-config", path, "-external-https"}, io.Discard); err == nil {
				t.Fatal("invalid merged configuration accepted")
			}
		})
	}
	path := writeConfig(t, `{"externalHttps":false,"developmentHttp":false}`)
	if _, err := Parse([]string{"-config", path}, io.Discard); err == nil {
		t.Fatal("explicit false transport fell back")
	}
	path = writeConfig(t, `{"externalHttps":true,"maxRequestBytes":10}`)
	for _, args := range [][]string{
		{"-max-request-bytes", "0"}, {"-report-root", ""}, {"-database-dsn", ""},
		{"-poll-max-age", "0s"}, {"-external-https=false"},
	} {
		if _, err := Parse(append([]string{"-config", path}, args...), io.Discard); err == nil {
			t.Fatal("invalid explicit CLI override fell back to file")
		}
	}
}

func TestFilePaths(t *testing.T) {
	path := writeConfig(t, `{"developmentHttp":true,"database":{"dsn":"data/registry.db"},"reportRoot":"data/reports"}`)
	got, err := Parse([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseDSN != filepath.Join(filepath.Dir(path), "data", "registry.db") ||
		got.ReportRoot != filepath.Join(filepath.Dir(path), "data", "reports") {
		t.Fatal("file paths were not resolved against config directory")
	}
	data, err := json.Marshal(map[string]any{
		"developmentHttp": true, "database": map[string]string{"dsn": got.DatabaseDSN},
		"reportRoot": got.ReportRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := Parse([]string{"-config", writeConfig(t, string(data))}, io.Discard)
	if err != nil || absolute != got {
		t.Fatal("absolute file paths changed")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, path)
	if err == nil {
		fromRelative, err := Parse([]string{"-config", relative}, io.Discard)
		if err != nil || fromRelative != got {
			t.Fatal("relative config path changed resolution")
		}
	}
	for _, dsn := range []string{"postgres://registry:password@localhost/registry", "host=localhost dbname=registry password=secret"} {
		path := writeConfig(t, `{"externalHttps":true,"database":{"engine":"postgres","dsn":"`+dsn+`"}}`)
		got, err := Parse([]string{"-config", path}, io.Discard)
		if err != nil || got.DatabaseDSN != dsn {
			t.Fatal("PostgreSQL DSN was reinterpreted as path")
		}
	}
	path = writeConfig(t, `{"developmentHttp":true,"database":{"dsn":"registry.db"}}`)
	got, err = Parse([]string{"-config", path, "-database-engine", "postgres"}, io.Discard)
	if err != nil || got.DatabaseDSN != "registry.db" {
		t.Fatal("final engine override was not used for path resolution")
	}
	path = writeConfig(t, `{"developmentHttp":true,"database":{"engine":"postgres","dsn":"registry.db"}}`)
	got, err = Parse([]string{"-config", path, "-database-engine", "sqlite"}, io.Discard)
	if err != nil || got.DatabaseDSN != filepath.Join(filepath.Dir(path), "registry.db") {
		t.Fatal("SQLite engine override did not resolve file-supplied path")
	}
	path = writeConfig(t, `{"developmentHttp":true,"database":{"dsn":"same"},"reportRoot":"same"}`)
	if _, err := Parse([]string{"-config", path}, io.Discard); err == nil {
		t.Fatal("database/report path collision accepted")
	}
}

func TestBootstrapFile(t *testing.T) {
	path := writeConfig(t, `{
		"listenAddress":"invalid", "externalHttps":true, "developmentHttp":true,
		"tls":{"futureSetting":true,"certFile":"missing.pem"},
		"futureFeature":{"enabled":true,"options":[null,1,{}]},
		"reportRoot":"", "pollMaxAge":"0s", "maxRequestBytes":0,
		"maxReportBytes":0, "readHeaderTimeout":"0s", "readTimeout":"0s",
		"writeTimeout":"0s", "idleTimeout":"0s", "shutdownTimeout":"0s", "probeTimeout":"0s",
		"database":{"futureSetting":null,"dsn":"registry.db"}, "startupTimeout":"3s", "sqliteBusyTimeout":"500ms"
	}`)
	got, name, err := ParseBootstrap([]string{"-config", path, "-name", "operator"}, io.Discard)
	if err != nil || name != "operator" || got.DatabaseDSN != filepath.Join(filepath.Dir(path), "registry.db") ||
		got.StartupTimeout != 3*time.Second || got.SQLiteBusyTimeout != 500*time.Millisecond {
		t.Fatalf("bootstrap file: %v", err)
	}
	if _, err := Parse([]string{"-config", path}, io.Discard); err == nil {
		t.Fatal("server accepted settings that only bootstrap may accept")
	}

	got, _, err = ParseBootstrap([]string{"-config", path, "-database-engine", "postgres",
		"-database-dsn", "host=localhost", "-startup-timeout", "10s", "-sqlite-busy-timeout", "5s"}, io.Discard)
	if err != nil || got.DatabaseEngine != "postgres" || got.DatabaseDSN != "host=localhost" ||
		got.StartupTimeout != 10*time.Second || got.SQLiteBusyTimeout != 5*time.Second {
		t.Fatalf("bootstrap overrides: %v", err)
	}
	for _, property := range []string{
		`"database":{"dsn":""}`, `"database":{"engine":"invalid"}`,
		`"startupTimeout":"0s"`, `"sqliteBusyTimeout":"0s"`,
	} {
		path := writeConfig(t, "{"+property+"}")
		if _, _, err := ParseBootstrap([]string{"-config", path}, io.Discard); err == nil {
			t.Fatal("bootstrap accepted invalid database settings")
		}
	}
}

func TestTLSFilePaths(t *testing.T) {
	path := writeConfig(t, `{
		"listenAddress":"0.0.0.0:8443",
		"tls":{"certFile":"certs/server.crt","keyFile":"certs/server.key"}
	}`)
	got, err := Parse([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := TLSSettings{
		CertFile: filepath.Join(filepath.Dir(path), "certs", "server.crt"),
		KeyFile:  filepath.Join(filepath.Dir(path), "certs", "server.key"),
	}
	if got.TLS == nil || *got.TLS != want || got.ExternalHTTPS || got.DevelopmentHTTP {
		t.Fatal("TLS paths or native mode not loaded")
	}
	data, err := json.Marshal(map[string]any{
		"tls": map[string]string{"certFile": want.CertFile, "keyFile": want.KeyFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := Parse([]string{"-config", writeConfig(t, string(data))}, io.Discard)
	if err != nil || absolute.TLS == nil || *absolute.TLS != want {
		t.Fatal("absolute TLS paths changed")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, path)
	if err == nil {
		fromRelative, err := Parse([]string{"-config", relative}, io.Discard)
		if err != nil || fromRelative.TLS == nil || *fromRelative.TLS != want {
			t.Fatal("relative config path changed TLS path resolution")
		}
	}
}

func TestTLSTransportValidation(t *testing.T) {
	for _, body := range []string{
		`{"tls":{}}`,
		`{"tls":{},"developmentHttp":true}`,
		`{"tls":{},"externalHttps":true}`,
		`{"tls":{"certFile":"server.crt"}}`,
		`{"tls":{"keyFile":"server.key"}}`,
		`{"tls":{"futureSetting":true,"certFile":"server.crt"}}`,
		`{"tls":{"certFile":"","keyFile":"server.key"}}`,
		`{"tls":{"certFile":"server.crt","keyFile":" "}}`,
		`{"tls":{"certFile":"server.crt","keyFile":"server.key"},"externalHttps":true}`,
		`{"tls":{"certFile":"server.crt","keyFile":"server.key"},"developmentHttp":true}`,
		`{"tls":{"certFile":"server.crt","keyFile":"server.key"},"externalHttps":true,"developmentHttp":true}`,
		`{"tls":{"certFile":"server.crt","keyFile":"server.key"},"listenAddress":"localhost:8443"}`,
		`{"tls":{"certFile":"server.crt","keyFile":"server.key"},"listenAddress":"127.0.0.1:65536"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := Parse([]string{"-config", writeConfig(t, body)}, io.Discard); err == nil {
				t.Fatal("invalid TLS settings accepted")
			}
		})
	}
	path := writeConfig(t, `{"tls":{"certFile":"server.crt","keyFile":"server.key"}}`)
	for _, mode := range []string{"-external-https", "-development-http"} {
		if _, err := Parse([]string{"-config", path, mode}, io.Discard); err == nil {
			t.Fatal("CLI override accepted incompatible TLS mode")
		}
	}
	path = writeConfig(t, `{
		"tls":{"certFile":"server.crt","keyFile":"server.key"},
		"externalHttps":true,"developmentHttp":true
	}`)
	if _, err := Parse([]string{"-config", path, "-external-https=false", "-development-http=false"}, io.Discard); err != nil {
		t.Fatalf("explicit false overrides did not select native TLS: %v", err)
	}
}
