package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func dependencyEntries(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["dependency"] != nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func TestDatabaseFailureDiagnostics(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		var logs bytes.Buffer
		_, store, admin, _, _ := configurationAPI(t, engine, dsn, &logs)
		injected := &pgconn.PgError{Code: "08006", Message: "private-driver-message", Detail: "private-parameters"}
		failed := &database.Store{Engine: engine, DB: testdb.ObserveQueries(t, store.DB, dsn,
			func(context.Context, string, []driver.NamedValue) error { return injected })}
		handler := New(failed, nil, nil, time.Second, time.Minute, 1024, 1024,
			slog.New(slog.NewJSONHandler(&logs, nil)))
		logs.Reset()
		response := apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 503)
		entries := dependencyEntries(t, &logs)
		if len(entries) != 1 || entries[0]["dependency"] != "database" || entries[0]["operation"] != "query" ||
			!strings.Contains(logs.String(), `"sqlstate":"08006"`) {
			t.Fatal("missing or duplicate database diagnostics", logs.String())
		}
		for _, secret := range []string{"private-driver-message", "private-parameters", admin.Key, dsn} {
			if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
				t.Fatal("private data leaked")
			}
		}
	})
}

func TestExpectedErrorsAndCancellationDoNotLogOutages(t *testing.T) {
	var logs bytes.Buffer
	api := &adminAPI{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	for _, err := range []error{database.ErrNotFound, database.ErrConfigurationNotFound, database.ErrReportNotFound, database.ErrUploadConflict, database.ErrReportCursor} {
		api.reportError(httptest.NewRecorder(), err)
	}
	api.machineCreationError(httptest.NewRecorder(), database.ErrMachineNameConflict)
	api.failure(httptest.NewRecorder(), context.Canceled)
	api.failure(httptest.NewRecorder(), sql.ErrTxDone)
	api.reportStorageError(httptest.NewRecorder(), "write", context.Canceled, context.Canceled)
	if len(dependencyEntries(t, &logs)) != 0 {
		t.Fatal("expected errors logged as outages", logs.String())
	}
	api.failure(httptest.NewRecorder(), context.DeadlineExceeded)
	if entries := dependencyEntries(t, &logs); len(entries) != 1 || !strings.Contains(logs.String(), `"timeout":true`) {
		t.Fatal("deadline not diagnosed", logs.String())
	}
}

func TestMissingStoredReportRecoversWithoutAffectingHealth(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		var logs bytes.Buffer
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, &logs)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		handler := New(db, files, nil, time.Second, time.Minute, 4096, 4096,
			slog.New(slog.NewJSONHandler(&logs, nil)))
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		cfg := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201))
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", reportBody(cfg.Revision, uuid.NewString(), rawSuccessReport), 201)
		stored := storedReportPaths(t, db, machine.ID)[0]
		content, err := os.ReadFile(filepath.Join(root, stored))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, stored)); err != nil {
			t.Fatal(err)
		}
		logs.Reset()
		response := apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 503)
		entries := dependencyEntries(t, &logs)
		if len(entries) != 1 || entries[0]["operation"] != "read" || entries[0]["errno"] != float64(syscall.ENOENT) {
			t.Fatal("missing storage diagnostics", logs.String())
		}
		for _, secret := range []string{root, stored, "private-dsc-result", key.Key} {
			if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
				t.Fatal("private storage data leaked")
			}
		}
		for _, endpoint := range []string{"/health/live", "/health/ready"} {
			apiRequest(t, handler, "", "GET", endpoint, "", 200)
		}
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200)
		if err := os.WriteFile(filepath.Join(root, stored), content, 0600); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 200)
	})
}

func TestReportSyncFailureLogging(t *testing.T) {
	var logs bytes.Buffer
	api := &adminAPI{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	err := &os.PathError{Op: "sync", Path: "/private/report", Err: syscall.EIO}
	api.reportStorageError(httptest.NewRecorder(), "write", err, err)
	entries := dependencyEntries(t, &logs)
	if len(entries) != 1 || entries[0]["operation"] != "sync" || entries[0]["errno"] != float64(syscall.EIO) {
		t.Fatal("missing or duplicate filesystem failure", logs.String())
	}
	if strings.Contains(logs.String(), "/private/report") {
		t.Fatal("path leaked")
	}
}

func TestDatabaseReselectFailurePreservesStorageResponse(t *testing.T) {
	var logs bytes.Buffer
	api := &adminAPI{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	response := httptest.NewRecorder()
	api.reportStorageError(response, "read", &pgconn.PgError{Code: "08006"}, os.ErrNotExist)
	entries := dependencyEntries(t, &logs)
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"code":"report_storage_unavailable"`) || len(entries) != 1 || entries[0]["dependency"] != "database" {
		t.Fatal("storage response or final failure classification changed", response.Body, logs.String())
	}
}
