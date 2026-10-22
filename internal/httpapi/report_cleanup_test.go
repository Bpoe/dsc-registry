package httpapi

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func storedReportPaths(t *testing.T, db *database.Store, machineID string) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT report_path FROM reports WHERE machine_id = $1 ORDER BY name`, machineID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestReportFileCleanupBothEngines(t *testing.T) {
	for _, operation := range []string{"replace", "configuration", "machine"} {
		for _, state := range []string{"present", "missing", "blocked"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				testdb.Engines(t, func(t *testing.T, engine, dsn string) {
					testReportFileCleanup(t, engine, dsn, operation, state)
				})
			})
		}
	}
}

func testReportFileCleanup(t *testing.T, engine, dsn, operation, state string) {
	t.Helper()
	var logs bytes.Buffer
	_, db, admin, machine, key := configurationAPI(t, engine, dsn, &logs)
	root := t.TempDir()
	files, err := reportstore.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	handler := New(db, files, time.Second, 5*time.Second, time.Second, 2048, 4096,
		slog.New(slog.NewJSONHandler(&logs, nil)))
	base := "/v1/machines/" + machine.ID
	configPath := base + "/configurations/docker"
	config := decodeResponse[database.ConfigurationResult](t,
		apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201))
	apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
		historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport), 201)
	if operation == "machine" {
		other := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/other", `{"template":{}}`, 201))
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("other", other.Revision, uuid.NewString(), rawSuccessReport), 201)
	}
	paths := storedReportPaths(t, db, machine.ID)
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal("first report reference has no file", err)
		}
	}
	old := filepath.Join(root, paths[0])
	if state != "present" {
		if err := os.Remove(old); err != nil {
			t.Fatal(err)
		}
	}
	if state == "blocked" {
		if err := os.Mkdir(old, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(old, "blocker"), []byte("not a report"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logs.Reset()
	switch operation {
	case "replace":
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport), 201)
	case "configuration":
		apiRequest(t, handler, admin.Key, "DELETE", configPath, "", 204)
	case "machine":
		apiRequest(t, handler, admin.Key, "DELETE", base, "", 204)
		apiRequest(t, handler, key.Key, "GET", "/v1/agent/configurations", "", 401)
	}
	current := storedReportPaths(t, db, machine.ID)
	if operation == "replace" {
		if len(current) != 1 || current[0] == paths[0] {
			t.Fatal("replacement did not retain only its own reference", current)
		}
		apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 200)
	} else if len(current) != 0 {
		t.Fatal("deletion retained report records", current)
	}
	for i, path := range paths {
		_, err := os.Stat(filepath.Join(root, path))
		if state == "blocked" && i == 0 {
			if err != nil {
				t.Fatal("blocked cleanup target unexpectedly disappeared", err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal("obsolete report not removed", path, err)
		}
	}
	warned := strings.Contains(logs.String(), `"level":"WARN","msg":"report file cleanup failed"`)
	if warned != (state == "blocked") {
		t.Fatal("cleanup warning missing or emitted for a missing file", logs.String())
	}
	if warned && (!strings.Contains(logs.String(), `"path":"`+paths[0]+`"`) ||
		!strings.Contains(logs.String(), `"error":`)) {
		t.Fatal("cleanup warning omitted relative path or error")
	}
	if strings.Contains(logs.String(), "private-dsc-result") {
		t.Fatal("cleanup logs disclosed report contents")
	}
}

func TestReportCleanupRollbackBothEngines(t *testing.T) {
	for _, operation := range []string{"replace", "configuration", "machine"} {
		for _, failure := range []string{"statement", "commit"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				testdb.Engines(t, func(t *testing.T, engine, dsn string) {
					var logs bytes.Buffer
					_, db, admin, machine, key := configurationAPI(t, engine, dsn, &logs)
					root := t.TempDir()
					files, err := reportstore.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}

					defer files.Close()
					handler := New(db, files, time.Second, 5*time.Second, time.Second, 2048, 4096,
						slog.New(slog.NewJSONHandler(&logs, nil)))
					base := "/v1/machines/" + machine.ID
					configPath := base + "/configurations/docker"
					config := decodeResponse[database.ConfigurationResult](t,
						apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201))
					first := decodeResponse[database.ReportMetadata](t,
						apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
							historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport), 201))
					oldPath := storedReportPaths(t, db, machine.ID)[0]
					table, event := "reports", "INSERT"
					if operation == "configuration" {
						table, event = "configurations", "DELETE"
					} else if operation == "machine" {
						table, event = "machines", "DELETE"
					}
					sqlExec := func(query string) {
						t.Helper()
						if _, err := db.DB.Exec(query); err != nil {
							t.Fatal(err)
						}
					}
					body := "SELECT RAISE(ABORT, 'injected persistence failure');"
					if failure == "commit" {
						sqlExec(`CREATE TABLE cleanup_parent(id INTEGER PRIMARY KEY)`)
						sqlExec(`CREATE TABLE cleanup_child(id INTEGER REFERENCES cleanup_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
						body = "INSERT INTO cleanup_child VALUES (1);"
					} else if engine == "postgres" {
						body = "RAISE EXCEPTION 'injected persistence failure';"
					}
					if engine == "sqlite" {
						sqlExec(`CREATE TRIGGER cleanup_fail AFTER ` + event + ` ON ` + table + ` BEGIN ` + body + ` END`)
					} else {
						sqlExec(`CREATE FUNCTION cleanup_fail() RETURNS trigger LANGUAGE plpgsql AS $$
							BEGIN ` + body + ` RETURN NULL; END $$`)
						sqlExec(`CREATE TRIGGER cleanup_fail AFTER ` + event + ` ON ` + table + `
							FOR EACH ROW EXECUTE FUNCTION cleanup_fail()`)
					}
					switch operation {
					case "replace":
						response := apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
							historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport), 503)
						if !strings.Contains(response.Body.String(), `"code":"database_unavailable"`) {
							t.Fatal("database failure was masked", response.Body)
						}
					case "configuration":
						apiRequest(t, handler, admin.Key, "DELETE", configPath, "", 503)
					case "machine":
						apiRequest(t, handler, admin.Key, "DELETE", base, "", 503)
					}
					if paths := storedReportPaths(t, db, machine.ID); len(paths) != 1 || paths[0] != oldPath {
						t.Fatal("failed operation changed committed reference", paths)
					}
					if got := apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+first.ID+"/content", "", 200); got.Body.String() != rawSuccessReport {
						t.Fatal("failed operation corrupted previous report")
					}
					entries, err := os.ReadDir(root)
					want := 1
					if operation == "replace" && failure == "commit" {
						want = 2 // The commit acknowledgement is conservatively uncertain.
					}
					if err != nil || len(entries) != want {
						t.Fatal("incorrect failure cleanup", entries, err)
					}
				})
			})
		}
	}
}

func TestFailedPersistenceCleanupWarningPreservesErrorBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		var logs bytes.Buffer
		_, store, _, machine, _ := configurationAPI(t, engine, dsn, &logs)
		files, err := reportstore.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		injected := errors.New("injected database failure")
		failed := &database.Store{Engine: engine, DB: testdb.ObserveQueries(t, store.DB, dsn,
			func(_ context.Context, query string, _ []driver.NamedValue) error {
				if strings.Contains(query, "COALESCE(MAX(sequence)") {
					return injected
				}
				return nil
			})}
		upload, err := report.Parse([]byte(historyBody("docker", uuid.NewString(), uuid.NewString(), rawSuccessReport)))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		result, persistenceErr := failed.PublishReport(ctx, machine.ID, "docker", upload,
			func() (string, error) { return files.Write(ctx, upload.Result) },
			func(path string) ([]byte, error) { return files.Read(ctx, path, 4096) })
		if !errors.Is(persistenceErr, injected) || len(result.CleanupPaths) != 1 {
			t.Fatal("original persistence error lost", result, persistenceErr)
		}
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		api := &adminAPI{reports: files, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		api.removeReportFiles(result.CleanupPaths)
		response := httptest.NewRecorder()
		api.reportError(response, persistenceErr)
		if response.Code != 503 || !strings.Contains(response.Body.String(), `"code":"database_unavailable"`) ||
			!errors.Is(persistenceErr, injected) {
			t.Fatal("cleanup failure masked original database failure", response.Body)
		}
		if !strings.Contains(logs.String(), `"msg":"report file cleanup failed"`) ||
			!strings.Contains(logs.String(), `"path":"`+result.CleanupPaths[0]+`"`) ||
			!strings.Contains(logs.String(), `"error":`) {
			t.Fatal("cleanup failure not logged with path and error")
		}
	})
}
