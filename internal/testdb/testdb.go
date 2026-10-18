// Package testdb provides isolated databases for opt-in PostgreSQL integration tests.
package testdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func Engines(t *testing.T, run func(*testing.T, string, string)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		run(t, "sqlite", filepath.Join(t.TempDir(), "registry.db"))
	})
	t.Run("postgres", func(t *testing.T) {
		run(t, "postgres", Postgres(t))
	})
}

func Postgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DSCREGISTRY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL integration requires DSCREGISTRY_TEST_POSTGRES_DSN pointing to a disposable test database")
	}
	settings, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test DSN")
	}
	db := stdlib.OpenDB(*settings)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	schema := "dscreg_test_" + uuid.New().String()
	quoted := pgx.Identifier{schema}.Sanitize()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("could not create isolated PostgreSQL test schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("could not remove isolated PostgreSQL test schema")
		}
	})
	if settings.RuntimeParams == nil {
		settings.RuntimeParams = make(map[string]string)
	}
	settings.RuntimeParams["search_path"] = quoted
	registered := stdlib.RegisterConnConfig(settings)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(registered) })
	return registered
}
