package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/migrations"
	"github.com/google/uuid"
)

func testStore(t *testing.T, engine, dsn string) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := Connect(ctx, engine, dsn, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	return store
}

func TestAdministrationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		store := testStore(t, engine, dsn)
		admin, err := store.Bootstrap(ctx, "operator")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uuid.Parse(admin.AdminID); err != nil {
			t.Fatal("administrator UUID not generated")
		}
		if _, err := store.Bootstrap(ctx, "other"); !errors.Is(err, ErrAlreadyBootstrapped) {
			t.Fatalf("repeated bootstrap: %v", err)
		}
		principal, err := store.Authenticate(ctx, admin.Key)
		if err != nil || principal.Role != apikey.Admin || principal.OwnerID != admin.AdminID || principal.KeyID != admin.KeyID {
			t.Fatalf("administrator principal: %+v, %v", principal, err)
		}
		var digest []byte
		if err := store.DB.QueryRowContext(ctx, "SELECT secret_hash FROM api_keys WHERE key_id = $1", admin.KeyID).Scan(&digest); err != nil ||
			len(digest) != 32 || !bytes.Equal(digest, apikey.Digest(admin.Key)) || bytes.Contains(digest, []byte(admin.Key)) {
			t.Fatal("credential was not stored as a digest")
		}
		first, err := store.CreateMachine(ctx, "first display name")
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.CreateMachine(ctx, "second display name")
		if err != nil || first.ID == second.ID {
			t.Fatalf("names used as identity: %v", err)
		}
		if _, err := uuid.Parse(first.ID); err != nil {
			t.Fatal("machine UUID not generated")
		}
		got, err := store.GetMachine(ctx, first.ID)
		if err != nil || got != first || got.ConfigurationRevision != 0 {
			t.Fatalf("get machine: %+v, %v", got, err)
		}
		page, err := store.ListMachines(ctx, "", 1)
		if err != nil || len(page) != 1 {
			t.Fatalf("first page: %+v, %v", page, err)
		}
		next, err := store.ListMachines(ctx, page[0].ID, 1)
		if err != nil || len(next) != 1 || next[0].ID == page[0].ID {
			t.Fatalf("next page: %+v, %v", next, err)
		}
		machineKey, err := store.IssueKey(ctx, apikey.Machine, first.ID, "agent")
		if err != nil {
			t.Fatal(err)
		}
		principal, err = store.Authenticate(ctx, machineKey.Key)
		if err != nil || principal.Role != apikey.Machine || principal.OwnerID != first.ID {
			t.Fatalf("machine principal: %+v, %v", principal, err)
		}
		keys, err := store.ListKeys(ctx, apikey.Machine, first.ID, "", 10)
		if err != nil || len(keys) != 1 || keys[0].Description != "agent" || keys[0].RevokedAt != nil {
			t.Fatalf("key metadata: %+v, %v", keys, err)
		}
		encoded, err := json.Marshal(keys)
		if err != nil || strings.Contains(string(encoded), machineKey.Key) || strings.Contains(string(encoded), "secret_hash") {
			t.Fatal("key metadata reveals credentials")
		}
		for _, value := range []string{
			"dsca_" + machineKey.Key[5:], "dscm_" + admin.Key[5:], machineKey.Key[:30] + strings.Repeat("A", 43),
		} {
			if _, err := store.Authenticate(ctx, value); !errors.Is(err, apikey.ErrInvalid) {
				t.Fatalf("forged credential accepted: %v", err)
			}
		}
		if err := store.RevokeKey(ctx, apikey.Machine, second.ID, machineKey.KeyID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-owner revoke: %v", err)
		}
		if _, err := store.Authenticate(ctx, machineKey.Key); err != nil {
			t.Fatal("cross-owner revoke changed key")
		}
		for range 2 {
			if err := store.RevokeKey(ctx, apikey.Machine, first.ID, machineKey.KeyID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.Authenticate(ctx, machineKey.Key); !errors.Is(err, apikey.ErrInvalid) {
			t.Fatalf("revoked key accepted: %v", err)
		}
		keys, err = store.ListKeys(ctx, apikey.Machine, first.ID, "", 10)
		if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
			t.Fatalf("revoked key metadata: %+v, %v", keys, err)
		}
		activeKey, err := store.IssueKey(ctx, apikey.Machine, first.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		rotation, err := store.IssueKey(ctx, apikey.Admin, admin.AdminID, "rotation")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeKey(ctx, apikey.Admin, admin.AdminID, admin.KeyID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authenticate(ctx, admin.Key); !errors.Is(err, apikey.ErrInvalid) {
			t.Fatalf("old administrator key accepted: %v", err)
		}
		if _, err := store.Authenticate(ctx, rotation.Key); err != nil {
			t.Fatal("replacement administrator key refused")
		}
		if got, err := store.GetMachine(ctx, first.ID); err != nil || got.ConfigurationRevision != 0 {
			t.Fatal("credential changes modified configuration revision")
		}
		if _, err := store.DeleteMachine(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authenticate(ctx, activeKey.Key); !errors.Is(err, apikey.ErrInvalid) {
			t.Fatalf("deleted machine key accepted: %v", err)
		}
		if _, err := store.GetMachine(ctx, first.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted machine present: %v", err)
		}
		if _, err := store.IssueKey(ctx, apikey.Machine, first.ID, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("orphan machine key: %v", err)
		}
		if _, err := store.IssueKey(ctx, apikey.Admin, uuid.NewString(), ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("HTTP must not create admin identities: %v", err)
		}
		if err := store.RevokeKey(ctx, apikey.Admin, admin.AdminID, rotation.KeyID); err != nil {
			t.Fatal(err)
		}
		recovery, err := store.Bootstrap(ctx, "ignored recovery name")
		if err != nil || recovery.AdminID != admin.AdminID {
			t.Fatalf("local recovery failed: %+v, %v", recovery, err)
		}
		if _, err := store.Authenticate(ctx, recovery.Key); err != nil {
			t.Fatal("recovery credential refused")
		}
		if err := store.DB.Close(); err != nil {
			t.Fatal(err)
		}
		store = testStore(t, engine, dsn)
		if _, err := store.Authenticate(ctx, recovery.Key); err != nil {
			t.Fatal("credential did not survive restart")
		}
		if _, err := store.GetMachine(ctx, second.ID); err != nil {
			t.Fatal("machine did not survive restart")
		}
		migrations := 4
		if engine == "postgres" {
			migrations = 5
		}
		assertCount(t, store.DB, "schema_migrations", migrations)
	})
}

func TestConcurrentBootstrapAndMachineDeletion(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		first := testStore(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, store := range []*Store{first, second} {
			go func() {
				<-start
				_, err := store.Bootstrap(ctx, "admin")
				results <- err
			}()
		}
		close(start)
		var successes, conflicts int
		for range 2 {
			switch err := <-results; {
			case err == nil:
				successes++
			case errors.Is(err, ErrAlreadyBootstrapped):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("bootstrap successes=%d conflicts=%d", successes, conflicts)
		}
		for range 10 {
			machine, err := first.CreateMachine(ctx, "host")
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var group sync.WaitGroup
			group.Add(2)
			go func() {
				defer group.Done()
				<-start
				_, err := first.IssueKey(ctx, apikey.Machine, machine.ID, "")
				if err != nil && !errors.Is(err, ErrNotFound) {
					t.Error(err)
				}
			}()
			go func() {
				defer group.Done()
				<-start
				if _, err := second.DeleteMachine(ctx, machine.ID); err != nil {
					t.Error(err)
				}
			}()
			close(start)
			group.Wait()
			var count int
			if err := first.DB.QueryRow("SELECT COUNT(*) FROM api_keys WHERE owner_type = 'machine' AND owner_id = $1", machine.ID).
				Scan(&count); err != nil || count != 0 {
				t.Fatalf("concurrent deletion left orphan key: %d, %v", count, err)
			}
		}
	})
}

func TestAdministrationFailureRollsBack(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		// Make key persistence fail after inserting the new administrator.
		if _, err := store.DB.Exec("ALTER TABLE api_keys ADD CONSTRAINT impossible CHECK (false)"); err != nil {
			if engine != "sqlite" {
				t.Fatal(err)
			}
			exec(t, store.DB, `CREATE TRIGGER fail_key BEFORE INSERT ON api_keys BEGIN SELECT RAISE(ABORT, 'injected'); END`)
		}
		if _, err := store.Bootstrap(context.Background(), "admin"); err == nil {
			t.Fatal("bootstrap failure not surfaced")
		}
		assertCount(t, store.DB, "admins", 0)
		assertCount(t, store.DB, "api_keys", 0)
	})
}

func TestPostgresMigrationRollbackAndVersion(t *testing.T) {
	dsn := testdb.Postgres(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec(t, raw, "CREATE TABLE configurations (existing TEXT)")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if db, err := OpenPostgres(ctx, dsn); db != nil || err == nil {
		t.Fatal("conflicting PostgreSQL migration accepted")
	}
	var table sql.NullString
	if err := raw.QueryRow("SELECT to_regclass('schema_migrations')::text").Scan(&table); err != nil || table.Valid {
		t.Fatalf("migration ledger was not rolled back: %v", err)
	}
	if err := raw.QueryRow("SELECT to_regclass('machines')::text").Scan(&table); err != nil || table.Valid {
		t.Fatalf("migration DDL was not rolled back: %v", err)
	}
	exec(t, raw, "INSERT INTO configurations VALUES ('preserved')")
	exec(t, raw, "DROP TABLE configurations")
	store := testStore(t, "postgres", dsn)
	exec(t, store.DB, "INSERT INTO schema_migrations VALUES (6, 'future')")
	if db, err := OpenPostgres(ctx, dsn); db != nil || err == nil {
		t.Fatal("future PostgreSQL schema accepted")
	}
}

func TestSQLitePR1UpgradePreservesAllData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, raw, migrations.SQLiteInitial)
	exec(t, raw, "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)")
	exec(t, raw, "INSERT INTO schema_migrations VALUES (1, '2026-10-18T00:00:00Z')")
	id := uuid.NewString()
	adminID := uuid.NewString()
	key, err := apikey.Generate(apikey.Admin)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, raw, "INSERT INTO machines VALUES ($1, 'preserved', 7, 'then')", id)
	exec(t, raw, "INSERT INTO admins VALUES ($1, 'operator', 'then')", adminID)
	exec(t, raw, `INSERT INTO api_keys VALUES ($1, 'admin', $2, $3, 'then', NULL, 'existing')`,
		key.ID, adminID, apikey.Digest(key.Secret))
	exec(t, raw, `INSERT INTO configurations VALUES ($1, 'one', 'content-revision', '{"resources":[]}', '{}', $2, 'then')`,
		id, make([]byte, 32))
	exec(t, raw, `INSERT INTO configuration_status
			(machine_id, name, configuration_revision, received_at, started_at, finished_at, outcome, report_path, last_upload_id)
			VALUES ($1, 'one', 'content-revision', 'then', 'then', 'then', 'failed', 'retained/report.json', 'upload')`, id)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store := testStore(t, "sqlite", path)
	for _, table := range []string{"machines", "admins", "api_keys", "configurations", "reports"} {
		assertCount(t, store.DB, table, 1)
	}
	assertCount(t, store.DB, "schema_migrations", 4)
	machine, err := store.GetMachine(context.Background(), id)
	if err != nil || machine.Name != "preserved" || machine.ConfigurationRevision != 7 {
		t.Fatal("PR 1 machine changed during upgrade")
	}
	if principal, err := store.Authenticate(context.Background(), key.Secret); err != nil || principal.OwnerID != adminID {
		t.Fatal("existing administrator credential did not survive upgrade")
	}
	var template, parameters, revision, report, ledgerTime string
	if err := store.DB.QueryRow("SELECT template_json, parameters_json, revision FROM configurations").
		Scan(&template, &parameters, &revision); err != nil || template != `{"resources":[]}` || parameters != "{}" || revision != "content-revision" {
		t.Fatal("PR 1 desired state changed during upgrade")
	}
	if err := store.DB.QueryRow("SELECT report_path FROM reports").Scan(&report); err != nil || report != "retained/report.json" {
		t.Fatal("PR 1 status pointer changed during upgrade")
	}
	if err := store.DB.QueryRow("SELECT applied_at FROM schema_migrations WHERE version = 1").Scan(&ledgerTime); err != nil ||
		ledgerTime != "2026-10-18T00:00:00Z" {
		t.Fatal("SQLite initial migration was reapplied")
	}
}

func TestConcurrentPostgresMigration(t *testing.T) {
	dsn := testdb.Postgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		db  *sql.DB
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			db, err := OpenPostgres(ctx, dsn)
			results <- result{db, err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		assertCount(t, result.db, "schema_migrations", 5)
		if err := result.db.Close(); err != nil {
			t.Error(err)
		}
	}
}
