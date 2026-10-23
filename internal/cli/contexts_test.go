package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateCLI(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"DSCREG_SERVER", "DSCREG_CONTEXT", "DSCREG_API_KEY", "DSCREG_API_KEY_FILE"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("DSCREG_CONFIG", path)
	return path
}

func runLocal(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := Execute(context.Background(), args, strings.NewReader(input), &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}

func TestContextsAndSavedCredentials(t *testing.T) {
	path := isolateCLI(t)
	for _, args := range [][]string{
		{"config", "set-context", "homelab", "--server", "https://registry.example.com"},
		{"config", "set-context", "staging", "--server", "https://staging.example.com"},
		{"config", "use-context", "homelab"},
	} {
		if code, _, err := runLocal(t, "", args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, err)
		}
	}
	if code, out, err := runLocal(t, "", "config", "current-context"); code != 0 || out != "homelab\n" {
		t.Fatalf("%d %q %s", code, out, err)
	}
	secret := "dsca_test.secret"
	if code, _, err := runLocal(t, secret+"\n", "auth", "login", "--api-key-stdin"); code != 0 {
		t.Fatalf("login %d %s", code, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("key leaked into context config")
	}
	code, out, diagnostic := runLocal(t, "", "config", "view", "-o", "json")
	if code != 0 || strings.Contains(out+diagnostic, secret) {
		t.Fatalf("view %d %s", code, diagnostic)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	credential := view["contexts"].(map[string]any)["homelab"].(map[string]any)["credentialFile"].(string)
	got, err := readPrivate(credential)
	if err != nil || strings.TrimSpace(got) != secret {
		t.Fatalf("saved key not protected/readable: %v", err)
	}
	if code, _, diagnostic := runLocal(t, "", "auth", "logout"); code != 0 {
		t.Fatalf("logout %d %s", code, diagnostic)
	}
	if _, err := os.Stat(credential); !os.IsNotExist(err) {
		t.Fatal("logout retained saved credential")
	}
	if code, _, err := runLocal(t, "", "config", "delete-context", "homelab"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, _, _ := runLocal(t, "", "config", "current-context"); code != 2 {
		t.Fatal("deleted current context remains selected")
	}
}

func TestConnectionPrecedence(t *testing.T) {
	isolateCLI(t)
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		if r.URL.Path == "/health/ready" {
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		_, _ = w.Write([]byte(`{"machines":[]}`))
	}))
	defer server.Close()
	if code, _, err := runLocal(t, "", "config", "set-context", "local", "--server", server.URL); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, _, err := runLocal(t, "", "config", "use-context", "local"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, _, err := runLocal(t, "dsca_saved.secret", "auth", "login", "--api-key-stdin"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := writePrivateNew(keyFile, []byte("dsca_file.secret")); err != nil {
		t.Fatal(err)
	}
	check := func(want string, args ...string) {
		t.Helper()
		if code, _, err := runLocal(t, "", args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, err)
		}
		if header != want {
			t.Fatalf("Authorization = %q, want %q", header, want)
		}
	}
	check("ApiKey dsca_saved.secret", "get", "machines")
	t.Setenv("DSCREG_API_KEY_FILE", keyFile)
	check("ApiKey dsca_file.secret", "get", "machines")
	t.Setenv("DSCREG_API_KEY", "dsca_env.secret")
	check("ApiKey dsca_env.secret", "get", "machines")
	check("ApiKey dsca_file.secret", "get", "machines", "--api-key-file", keyFile)
	t.Setenv("DSCREG_SERVER", "http://127.0.0.1:1")
	check("ApiKey dsca_env.secret", "get", "machines", "--server", server.URL)
	check("", "ping", "--server", server.URL)
}

func TestOfflineCommandsAndValidation(t *testing.T) {
	isolateCLI(t)
	for _, args := range [][]string{{"--version"}, {"version"}, {"--help"}, {"help", "apply"}, {"help", "configuration"}, {"completion", "powershell"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}} {
		if code, out, err := runLocal(t, "", args...); code != 0 || out == "" {
			t.Fatalf("%v: %d %q %s", args, code, out, err)
		}
	}
	for _, args := range [][]string{{"get", "machines", "--output", "bogus"}, {"get", "machines", "--limit", "1001"}, {"get", "machines", "--timeout", "0s"}, {"get", "machines", "--quiet", "--verbose"}, {"bogus"}, {"get", "machines", "--unknown"}, {"config", "set-context", "bad", "--server", "https://user:secret@example.com"}} {
		if code, _, err := runLocal(t, "", args...); code != 2 {
			t.Fatalf("%v: code %d %s", args, code, err)
		}
	}
	if code, _, _ := runLocal(t, "", "get", "machines"); code != 2 {
		t.Fatal("unconfigured server silently selected")
	}
}

func TestPrivateFilesNeverOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := writePrivateNew(path, []byte("original")); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateNew(path, []byte("replacement")); err == nil {
		t.Fatal("secret overwritten")
	}
	if data, err := readPrivate(path); err != nil || data != "original" {
		t.Fatalf("%q %v", data, err)
	}
}

func TestContextAndConfigSelectionPrecedence(t *testing.T) {
	config := isolateCLI(t)
	var selected string
	start := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			selected = name
			_, _ = w.Write([]byte(`{"machines":[]}`))
		}))
	}
	first, second := start("homelab"), start("staging")
	defer first.Close()
	defer second.Close()
	t.Setenv("DSCREG_API_KEY", "dsca_env.secret")
	for _, args := range [][]string{
		{"config", "set-context", "homelab", "--server", first.URL},
		{"config", "set-context", "staging", "--server", second.URL},
		{"config", "use-context", "homelab"},
	} {
		if code, _, err := runLocal(t, "", args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, err)
		}
	}
	check := func(want string, args ...string) {
		t.Helper()
		if code, _, err := runLocal(t, "", args...); code != 0 || selected != want {
			t.Fatalf("%v: %d %s, selected %q", args, code, err, selected)
		}
	}
	check("homelab", "get", "machines")
	t.Setenv("DSCREG_CONTEXT", "staging")
	check("staging", "get", "machines")
	check("homelab", "get", "machines", "--context", "homelab")
	t.Setenv("DSCREG_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	check("homelab", "get", "machines", "--config", config, "--context", "homelab")
}

func TestInvalidConfigAndCredentialsAreRedacted(t *testing.T) {
	path := isolateCLI(t)
	if err := os.WriteFile(path, []byte("unexpected: sensitive-config-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, err := runLocal(t, "", "config", "view"); code != 2 || strings.Contains(out+err, "sensitive-config-value") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code, _, err := runLocal(t, "", "config", "set-context", "local", "--server", "https://registry.example.com"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, _, err := runLocal(t, "", "config", "use-context", "local"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, out, err := runLocal(t, "dscm_sensitive.machine-key", "auth", "login", "--api-key-stdin"); code != 2 || strings.Contains(out+err, "dscm_sensitive") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	if code, _, err := runLocal(t, "dsca_saved.secret", "auth", "login", "--api-key-stdin"); code != 0 {
		t.Fatalf("%d %s", code, err)
	}
	if code, _, err := runLocal(t, "", "config", "set-context", "local", "--server", "https://different.example.com"); code != 2 {
		t.Fatalf("changed credential destination: %d %s", code, err)
	}
}
