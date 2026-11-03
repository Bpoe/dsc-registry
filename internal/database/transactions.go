package database

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type transaction struct {
	*sql.Tx
	store     *Store
	operation string
	start     time.Time
	done      bool
}

func (s *Store) begin(ctx context.Context, operation string) (*transaction, error) {
	start := time.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logTransaction(operation, start, "begin_error")
		return nil, err
	}
	return &transaction{Tx: tx, store: s, operation: operation, start: start}, nil
}

func (tx *transaction) Commit() error {
	err := tx.Tx.Commit()
	outcome := "committed"
	if err != nil {
		outcome = "commit_error"
	}
	tx.finish(outcome)
	return err
}

func (tx *transaction) Rollback() error {
	err := tx.Tx.Rollback()
	outcome := "rolled_back"
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		outcome = "rollback_error"
		// Rollback errors are usually ignored by deferred callers, so diagnose
		// this distinct failure here rather than losing it during propagation.
		if tx.store.Logger != nil {
			var pg *pgconn.PgError
			code := ""
			if errors.As(err, &pg) {
				code = pg.Code
			}
			tx.store.Logger.Warn("database rollback failed", "dependency", "database", "operation", tx.operation, "sqlstate", code)
		}
	}
	tx.finish(outcome)
	return err
}

func (tx *transaction) finish(outcome string) {
	if !tx.done {
		tx.done = true
		tx.store.logTransaction(tx.operation, tx.start, outcome)
	}
}

func (s *Store) logTransaction(operation string, start time.Time, outcome string) {
	if s.Logger == nil {
		return
	}
	// Keep outcome/timing telemetry; application boundaries log failure details.
	stats := s.DB.Stats()
	s.Logger.Log(context.Background(), slog.LevelInfo, "database transaction",
		"engine", s.Engine, "operation", operation, "outcome", outcome,
		"duration", time.Since(start), "pool_in_use", stats.InUse,
		"pool_wait_count", stats.WaitCount, "pool_wait_duration", stats.WaitDuration)
}
