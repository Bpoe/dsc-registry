package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/httpapi"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func TestRegistryCLIEndToEndBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		isolateCLI(t)
		ctx := context.Background()
		db, err := database.Connect(ctx, engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		reports, err := reportstore.Open(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reports.Close() })
		admin, err := db.Bootstrap(ctx, "operator")
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(httpapi.New(db, reports, time.Second, 5*time.Second, time.Minute, 1<<20, 16<<20, slog.New(slog.NewTextHandler(io.Discard, nil))))
		defer server.Close()
		t.Setenv("DSCREG_SERVER", server.URL)
		t.Setenv("DSCREG_API_KEY", admin.Key)
		run := func(args ...string) string {
			t.Helper()
			code, out, err := runLocal(t, "", args...)
			if code != 0 {
				t.Fatalf("%v: exit %d %s", args, code, err)
			}
			return out
		}
		run("create", "machine", "apphost01")
		if code, _, _ := runLocal(t, "", "create", "machine", "APPHOST01"); code != 4 {
			t.Fatal("name conflict did not return exit 4")
		}
		plain := filepath.Join(t.TempDir(), "docker.yaml")
		if err := os.WriteFile(plain, []byte("resources: []\nlarge: 9007199254740993\n"), 0600); err != nil {
			t.Fatal(err)
		}
		parameters := filepath.Join(t.TempDir(), "docker.parameters.json")
		if err := os.WriteFile(parameters, []byte(`{"secret":"sensitive-parameter"}`), 0600); err != nil {
			t.Fatal(err)
		}
		apply := []string{"apply", "configuration", "docker", "--machine", "APPHOST01", "-f", plain, "--parameters-file", parameters}
		if out := run(apply...); !strings.Contains(out, "created") {
			t.Fatalf("creation %q", out)
		}
		var first map[string]json.RawMessage
		if err := json.Unmarshal([]byte(run("get", "configuration", "docker", "--machine", "apphost01", "-o", "json")), &first); err != nil {
			t.Fatal(err)
		}
		if out := run(apply...); !strings.Contains(out, "unchanged") {
			t.Fatalf("no-op %q", out)
		}
		var second map[string]json.RawMessage
		if err := json.Unmarshal([]byte(run("get", "configuration", "docker", "--machine", "apphost01", "-o", "json")), &second); err != nil {
			t.Fatal(err)
		}
		if string(first["revision"]) != string(second["revision"]) || string(first["machineConfigurationRevision"]) != string(second["machineConfigurationRevision"]) {
			t.Fatal("no-op advanced revisions")
		}
		export := run("export", "configuration", "docker", "--machine", "apphost01")
		manifest := filepath.Join(t.TempDir(), "export.yaml")
		if err := os.WriteFile(manifest, []byte(export), 0600); err != nil {
			t.Fatal(err)
		}
		if out := run("apply", "-f", manifest); !strings.Contains(out, "unchanged") {
			t.Fatalf("export round trip %q", out)
		}
		for _, args := range [][]string{
			{"describe", "machine", "apphost01"}, {"describe", "configuration", "docker", "--machine", "apphost01"},
		} {
			out := run(args...)
			if strings.Contains(out, "sensitive-parameter") || strings.Contains(out, "resources:") {
				t.Fatal("describe leaked desired document")
			}
		}
		var machine database.Machine
		if err := json.Unmarshal([]byte(run("get", "machine", "apphost01", "-o", "json")), &machine); err != nil {
			t.Fatal(err)
		}
		key, err := db.IssueKey(ctx, "machine", machine.ID, "integration")
		if err != nil {
			t.Fatal(err)
		}
		var revision string
		if err := json.Unmarshal(first["revision"], &revision); err != nil {
			t.Fatal(err)
		}
		raw := `{ "schemaVersion":1,"configuration":"custom/docker.yaml","operation":"test","startedAt":"2026-10-23T10:00:00Z","finishedAt":"2026-10-23T10:00:01Z","durationMs":1000,"outcome":"succeeded","dscResult":{"drift":true,"number":9007199254740993},"opaque":"sensitive-report" }`
		body := `{"configurationName":"docker","configurationRevision":"` + revision + `","uploadId":"` + uuid.NewString() + `","result":` + raw + `}`
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/agent/reports", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "ApiKey "+key.Key)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 201 {
			t.Fatalf("report upload %d %s %v", response.StatusCode, data, err)
		}
		var receipt struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		got := run("get", "report", receipt.ID, "--machine", "apphost01", "--content", "-o", "raw")
		if got != raw {
			t.Fatalf("raw report bytes changed:\n%q\nwant:\n%q", got, raw)
		}
		out := run("describe", "configuration", "docker", "--machine", "apphost01")
		if !strings.Contains(out, "succeeded") || strings.Contains(out, "sensitive-report") || strings.Contains(out, "compliant") {
			t.Fatalf("unsafe describe %q", out)
		}
		if err := os.WriteFile(plain, []byte("resources: []\nlarge: 9007199254740994\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if out := run(apply...); !strings.Contains(out, "configured") {
			t.Fatalf("changed apply %q", out)
		}
		out = run("describe", "machine", "apphost01")
		if !strings.Contains(out, "stale") {
			t.Fatalf("missing stale observation %q", out)
		}
		out = run("describe", "configuration", "docker", "--machine", "apphost01")
		if !strings.Contains(out, "stale") || !strings.Contains(out, "Latest retained report is for a previous revision.") || strings.Contains(out, "unreported") {
			t.Fatalf("configuration lost stale report state: %q", out)
		}
		run("delete", "configuration", "docker", "--machine", "apphost01", "--yes")
		run("delete", "configuration", "docker", "--machine", "apphost01", "--yes", "--ignore-not-found")
		run("delete", "machine", "apphost01", "--yes")
	})
}
