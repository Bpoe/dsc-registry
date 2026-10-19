package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Bpoe/dsc-registry/migrations"
	_ "modernc.org/sqlite"
)

func Open(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if busyTimeout <= 0 || busyTimeout > time.Minute || busyTimeout%time.Millisecond != 0 {
		return nil, errors.New("invalid SQLite busy timeout")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite path: %w", err)
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close new SQLite file: %w", err)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create SQLite file: %w", err)
	}
	query := url.Values{
		"_pragma": {
			"foreign_keys(1)",
			fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()),
			"synchronous(FULL)",
		},
		"_txlock": {"immediate"},
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(absolute)}).EscapedPath() + "?" + query.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := initialize(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func initialize(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect SQLite: %w", err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if mode != "wal" {
		return errors.New("SQLite database must support WAL on a local filesystem")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER NOT NULL PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	var count, version int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations").
		Scan(&count, &version); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if count != version || version < 0 || version > 3 {
		return errors.New("unsupported SQLite schema version or migration ledger")
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, migrations.SQLiteInitial); err != nil {
			return fmt.Errorf("apply SQLite migration 1: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record SQLite migration 1: %w", err)
		}
	}
	if version < 2 {
		if _, err := tx.ExecContext(ctx, migrations.SQLiteMachineNames); err != nil {
			return fmt.Errorf("apply SQLite migration 2: %w", err)
		}
		if err := backfillMachineNames(ctx, tx); err != nil {
			return fmt.Errorf("backfill SQLite machine names: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			2, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record SQLite migration 2: %w", err)
		}
	}
	if version < 3 {
		if _, err := tx.ExecContext(ctx, migrations.SQLiteReportHistory); err != nil {
			return fmt.Errorf("apply SQLite migration 3: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			3, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record SQLite migration 3: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite migration: %w", err)
	}
	return nil
}
