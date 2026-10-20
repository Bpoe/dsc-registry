package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/unicode/norm"
)

func TestFoldMachineName(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"Docker-01", "docker-01"},
		{"\u00c9", "\u00e9"},
		{"E\u0301", "\u00e9"},
		{"Stra\u00dfe", "strasse"},
		{"\u03a3", "\u03c3"},
		{"\u03c2", "\u03c3"},
		{"\u01f0", "\u01f0"}, // Folding decomposes this character before the final NFC.
		{"\uff21", "\uff41"},
		{"\u0130", "i\u0307"},
		{"\u0131", "\u0131"},
		{"literal_%", "literal_%"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := foldMachineName(test.name)
			if got != test.want || !norm.NFC.IsNormalString(got) || foldMachineName(got) != got {
				t.Fatalf("fold %q = %q, want stable NFC %q", test.name, got, test.want)
			}
		})
	}
	for _, pair := range [][2]string{{"resume", "r\u00e9sum\u00e9"}, {"A", "\uff21"}, {"I", "\u0130"}, {"I", "\u0131"}} {
		if foldMachineName(pair[0]) == foldMachineName(pair[1]) {
			t.Fatalf("distinct names folded together: %q", pair)
		}
	}
}

func TestMachineNamesBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx := context.Background()
		for _, pair := range [][2]string{
			{"Docker-01", "DOCKER-01"},
			{"\u00c9tage", "e\u0301TAGE"},
			{"A\u030aring", "\u00c5RING"},
			{"Stra\u00dfe", "STRASSE"},
			{"sigma-\u03c2", "SIGMA-\u03a3"},
			{"literal_%_host", "LITERAL_%_HOST"},
		} {
			machine, err := store.CreateMachine(ctx, pair[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range pair {
				got, err := store.GetMachineByName(ctx, name)
				if err != nil || got != machine {
					t.Fatalf("lookup %q: %+v, %v", name, got, err)
				}
				if _, err := store.CreateMachine(ctx, name); !errors.Is(err, ErrMachineNameConflict) {
					t.Fatalf("duplicate %q: %v", name, err)
				}
			}
			var key string
			if err := store.DB.QueryRow("SELECT name_folded FROM machines WHERE id = $1", machine.ID).Scan(&key); err != nil ||
				key != foldMachineName(pair[0]) {
				t.Fatalf("stored name key differs from lookup: %q, %v", key, err)
			}
		}
		for _, name := range []string{"Docker", "Docker-01-extra", "Dock%", "literal_XX_host", "missing"} {
			if _, err := store.GetMachineByName(ctx, name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("non-exact name %q matched: %v", name, err)
			}
		}
		for _, name := range []string{"resume", "r\u00e9sum\u00e9", "A", "\uff21", "I", "\u0130", "\u0131"} {
			if _, err := store.CreateMachine(ctx, name); err != nil {
				t.Fatalf("distinct name %q conflicts: %v", name, err)
			}
		}
		machine, err := store.CreateMachine(ctx, "independent")
		if err != nil {
			t.Fatal(err)
		}
		exec(t, store.DB, "UPDATE machines SET name = name, name_folded = name_folded, configuration_revision = 7 WHERE id = $1", machine.ID)
		machine.ConfigurationRevision = 7
		if got, err := store.GetMachineByName(ctx, "INDEPENDENT"); err != nil || got != machine {
			t.Fatal("unchanged-name update or lookup changed machine", got, err)
		}
		_, err = store.DB.Exec("UPDATE machines SET name = $1, name_folded = $2 WHERE id = $3",
			"docker-01", foldMachineName("docker-01"), machine.ID)
		if !machineNameConflict(err) {
			t.Fatalf("database allowed conflicting name/key update: %v", err)
		}
		if got, err := store.GetMachine(ctx, machine.ID); err != nil || got != machine {
			t.Fatal("conflicting update changed original machine", got, err)
		}
		insert := "INSERT INTO machines(id, name, created_at, name_folded) VALUES ($1, $2, 'then', $3)"
		_, err = store.DB.Exec(insert, uuid.NewString(), "DOCKER-01", foldMachineName("DOCKER-01"))
		if !machineNameConflict(err) {
			t.Fatalf("database did not enforce name uniqueness: %v", err)
		}
		_, err = store.DB.Exec(insert, machine.ID, "different name", foldMachineName("different name"))
		if err == nil || machineNameConflict(err) {
			t.Fatalf("primary-key conflict misclassified as a name conflict: %v", err)
		}
		for _, key := range []any{nil, ""} {
			if _, err := store.DB.Exec(insert, uuid.NewString(), "invalid key", key); err == nil || machineNameConflict(err) {
				t.Fatalf("missing/empty name key accepted or misclassified: %v", err)
			}
			if _, err := store.DB.Exec("UPDATE machines SET name_folded = $1 WHERE id = $2", key, machine.ID); err == nil {
				t.Fatal("name key cleared on update")
			}
		}
		if _, err := store.DB.Exec("INSERT INTO machines(id, name, created_at) VALUES ($1, 'missing key', 'then')", uuid.NewString()); err == nil {
			t.Fatal("omitted name key accepted")
		}
		if err := store.DeleteMachine(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetMachineByName(ctx, "INDEPENDENT"); !errors.Is(err, ErrNotFound) {
			t.Fatal("deleted machine still resolves", err)
		}
		recreated, err := store.CreateMachine(ctx, "Independent")
		if err != nil || recreated.ID == machine.ID {
			t.Fatal("name reuse failed or reused UUID", err)
		}
	})
}

func TestMachineNameConflictClassification(t *testing.T) {
	for _, err := range []error{
		nil, errors.New("unique constraint"),
		&pgconn.PgError{Code: "23505", ConstraintName: "machines_pkey"},
		&pgconn.PgError{Code: "23514", ConstraintName: "machines_name_folded_required"},
	} {
		if machineNameConflict(err) {
			t.Fatalf("unrelated error classified as name conflict: %v", err)
		}
	}
	err := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: "machines_name_folded_unique"})
	if !machineNameConflict(err) {
		t.Fatal("wrapped name conflict not recognized")
	}
}

func machineNamesOldDatabase(t *testing.T, engine, dsn string) (*sql.DB, int) {
	t.Helper()
	driver, initial, version := "sqlite", migrations.SQLiteInitial, 1
	if engine == "postgres" {
		driver, initial, version = "pgx", migrations.PostgresInitial, 2
	}
	raw, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	exec(t, raw, initial)
	if engine == "postgres" {
		exec(t, raw, migrations.PostgresLosslessJSON)
	}
	exec(t, raw, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)")
	for v := 1; v <= version; v++ {
		exec(t, raw, "INSERT INTO schema_migrations VALUES ($1, 'original')", v)
	}
	return raw, version
}

func TestMachineNameMigrationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		raw, version := machineNamesOldDatabase(t, engine, dsn)
		machines := make([]Machine, 103)
		for i := range machines {
			machines[i] = Machine{ID: uuid.NewString(), Name: fmt.Sprintf("host-%03d-E\u0301", i), ConfigurationRevision: 7}
			exec(t, raw, "INSERT INTO machines VALUES ($1, $2, 7, 'then')", machines[i].ID, machines[i].Name)
		}
		for range 2 {
			store := testStore(t, engine, dsn)
			assertCount(t, store.DB, "schema_migrations", version+3)
			for i, machine := range machines {
				got, err := store.GetMachineByName(context.Background(), fmt.Sprintf("HOST-%03d-\u00e9", i))
				if err != nil || got != machine {
					t.Fatalf("migration/reopen changed machine: %+v, %v", got, err)
				}
			}
			if err := store.DB.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestMachineNameMigrationCollisionRollsBack(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		raw, version := machineNamesOldDatabase(t, engine, dsn)
		firstID, secondID := uuid.NewString(), uuid.NewString()
		exec(t, raw, "INSERT INTO machines VALUES ($1, $2, 7, 'then')", firstID, "\u00c9")
		exec(t, raw, "INSERT INTO machines VALUES ($1, $2, 9, 'then')", secondID, "e\u0301")
		if store, err := Connect(context.Background(), engine, dsn, time.Second); err == nil {
			store.DB.Close()
			t.Fatal("colliding legacy names were accepted")
		}
		assertCount(t, raw, "schema_migrations", version)
		assertCount(t, raw, "machines", 2)
		if _, err := raw.Exec("SELECT name_folded FROM machines"); err == nil {
			t.Fatal("failed migration retained schema changes")
		}
		for id, want := range map[string]string{firstID: "\u00c9", secondID: "e\u0301"} {
			var got string
			if err := raw.QueryRow("SELECT name FROM machines WHERE id = $1", id).Scan(&got); err != nil || got != want {
				t.Fatal("failed migration changed original name", err)
			}
		}
		exec(t, raw, "DELETE FROM machines WHERE id = $1", secondID)
		store := testStore(t, engine, dsn)
		got, err := store.GetMachineByName(context.Background(), "e\u0301")
		if err != nil || got.ID != firstID || got.ConfigurationRevision != 7 {
			t.Fatal("migration could not retry after explicit collision removal", got, err)
		}
	})
}
