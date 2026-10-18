package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Bpoe/dsc-registry/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func OpenPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection settings")
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := initializePostgres(ctx, db); err != nil {
		_ = db.Close()
		// Driver errors may include connection strings or SQL data.
		return nil, errors.New("PostgreSQL connection or migration failed")
	}
	return db, nil
}

func initializePostgres(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// All replicas serialize schema initialization, including ledger creation.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(7286519401)"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER NOT NULL PRIMARY KEY, applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var count, version int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations").
		Scan(&count, &version); err != nil {
		return err
	}
	if count != version || version < 0 || version > 1 {
		return errors.New("unsupported PostgreSQL schema version or migration ledger")
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, migrations.PostgresInitial); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES ($1, $2)",
			1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Store struct {
	DB     *sql.DB
	Engine string
}

func Connect(ctx context.Context, engine, dsn string, busyTimeout time.Duration) (*Store, error) {
	var db *sql.DB
	var err error
	switch engine {
	case "sqlite":
		db, err = Open(ctx, dsn, busyTimeout)
	case "postgres":
		db, err = OpenPostgres(ctx, dsn)
	default:
		return nil, errors.New("unsupported database engine")
	}
	if err != nil {
		return nil, err
	}
	return &Store{DB: db, Engine: engine}, nil
}
