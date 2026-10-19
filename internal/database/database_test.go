package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPersistsAndConfiguresConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry # percent%.db")
	db := openTestDB(t, path)
	exec(t, db, "INSERT INTO machines VALUES ($1, $2, $3, $4)",
		"550e8400-e29b-41d4-a716-446655440000", "display only", 0, "2026-10-18T00:00:00Z")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestDB(t, path)
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM machines").Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted machines: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = 1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration count=%d, error=%v", count, err)
	}
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("SQLite must use one pooled connection")
	}
	// Discard the initialized connection to verify per-connection pragmas.
	db.SetMaxIdleConns(0)
	for _, pragma := range []struct {
		query string
		want  string
	}{
		{"PRAGMA journal_mode", "wal"}, {"PRAGMA foreign_keys", "1"},
		{"PRAGMA busy_timeout", "100"}, {"PRAGMA synchronous", "2"},
	} {
		var got string
		if err := db.QueryRow(pragma.query).Scan(&got); err != nil || got != pragma.want {
			t.Fatalf("%s = %q, want %q; error=%v", pragma.query, got, pragma.want, err)
		}
	}
}

func TestSchemaIntegrityAndCascade(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		checkSchemaIntegrityAndCascade(t, store.DB)
	})
}

func checkSchemaIntegrityAndCascade(t *testing.T, db *sql.DB) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	now := "2026-10-18T00:00:00Z"
	exec(t, db, "INSERT INTO machines VALUES ($1, $2, $3, $4)", id, "duplicate name allowed", 0, now)
	exec(t, db, "INSERT INTO machines VALUES ($1, $2, $3, $4)", "second-id", "duplicate name allowed", 0, now)
	exec(t, db, "INSERT INTO admins VALUES ($1, $2, $3)", id, "admin", now)
	keySQL := "INSERT INTO api_keys(key_id, owner_type, owner_id, secret_hash, created_at) VALUES ($1, $2, $3, $4, $5)"
	exec(t, db, keySQL, "machine-key", "machine", id, make([]byte, 32), now)
	exec(t, db, keySQL, "admin-key", "admin", id, make([]byte, 32), now)
	configSQL := "INSERT INTO configurations VALUES ($1, $2, $3, $4, $5, $6, $7)"
	exec(t, db, configSQL, id, "one", "content-revision", "{}", nil, make([]byte, 32), now)
	exec(t, db, configSQL, id, "two", "another-revision", "{}", "{}", make([]byte, 32), now)
	exec(t, db, `INSERT INTO reports (
		id,sequence,machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,report_path,upload_id
	) VALUES ('report-id',1,$1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, "one", "content-revision", now, now, now, "failed", "uuid/report.json", "upload-id")
	for name, statement := range map[string]struct {
		query string
		args  []any
	}{
		"missing machine owner": {keySQL, []any{"bad", "machine", "missing", make([]byte, 32), now}},
		"missing admin owner":   {keySQL, []any{"bad", "admin", "missing", make([]byte, 32), now}},
		"owner type":            {keySQL, []any{"bad", "other", id, make([]byte, 32), now}},
		"short digest":          {keySQL, []any{"bad", "machine", id, []byte("secret"), now}},
		"owner update":          {"UPDATE api_keys SET owner_id = $1 WHERE key_id = $2", []any{"missing", "machine-key"}},
		"role update":           {"UPDATE api_keys SET owner_type = $1 WHERE key_id = $2", []any{"other", "machine-key"}},
		"immutable machine":     {"UPDATE machines SET id = $1 WHERE id = $2", []any{"new-id", id}},
		"immutable admin":       {"UPDATE admins SET id = $1 WHERE id = $2", []any{"new-id", id}},
		"foreign key":           {configSQL, []any{"missing", "three", "r", "{}", nil, make([]byte, 32), now}},
		"duplicate name":        {configSQL, []any{id, "one", "r", "{}", nil, make([]byte, 32), now}},
		"array template":        {configSQL, []any{id, "three", "r", "[]", nil, make([]byte, 32), now}},
		"null parameters JSON":  {configSQL, []any{id, "three", "r", "{}", "null", make([]byte, 32), now}},
		"bad JSON":              {configSQL, []any{id, "three", "r", "{", nil, make([]byte, 32), now}},
		"orphan report":         {"UPDATE reports SET machine_id = $1", []any{"missing"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.Exec(statement.query, statement.args...); err == nil {
				t.Fatal("constraint was not enforced")
			}
		})
	}
	exec(t, db, "DELETE FROM configurations WHERE machine_id = $1 AND name = $2", id, "one")
	assertCount(t, db, "reports", 1)
	assertCount(t, db, "configurations", 1)
	exec(t, db, "DELETE FROM machines WHERE id = $1", id)
	assertCount(t, db, "configurations", 0)
	assertCount(t, db, "reports", 0)
	assertCount(t, db, "api_keys", 1)
	exec(t, db, "DELETE FROM admins WHERE id = $1", id)
	assertCount(t, db, "api_keys", 0)
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d, want=%d, error=%v", table, count, want, err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec(t, raw, "CREATE TABLE configurations (existing TEXT)")
	db, err := Open(context.Background(), path, 100*time.Millisecond)
	if db != nil || err == nil {
		t.Fatalf("conflicting schema accepted: db=%v, error=%v", db, err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('machines', 'schema_migrations')`).
		Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration did not roll back: count=%d, error=%v", count, err)
	}
	exec(t, raw, "INSERT INTO configurations VALUES ($1)", "preserved")
}

func TestRejectFutureSchemaAndCanceledStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, raw, "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)")
	exec(t, raw, "INSERT INTO schema_migrations VALUES (3, 'future')")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(context.Background(), path, time.Second); db != nil || err == nil {
		t.Fatalf("future schema accepted: db=%v, error=%v", db, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if db, err := Open(ctx, path, time.Second); db != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup: db=%v, error=%v", db, err)
	}
	if db, err := Open(context.Background(), path, 0); db != nil || err == nil {
		t.Fatalf("invalid busy timeout accepted: db=%v, error=%v", db, err)
	}
}

func TestBusyTimeoutIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	first := openTestDB(t, path)
	second := openTestDB(t, path)
	tx, err := first.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start := time.Now()
	if _, err := second.Exec("INSERT INTO admins VALUES ('id', 'name', 'now')"); err == nil {
		t.Fatal("second writer unexpectedly acquired the transaction lock")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("busy timeout was not bounded: %v", elapsed)
	}
}
