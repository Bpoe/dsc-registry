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
	ctx       context.Context
}

func (s *Store) begin(ctx context.Context, operation string) (*transaction, error) {
	start := time.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logTransaction(ctx, operation, start, "begin_error", err)
		return nil, err
	}
	return &transaction{Tx: tx, store: s, operation: operation, start: start, ctx: ctx}, nil
}

func (tx *transaction) Commit() error {
	err := tx.Tx.Commit()
	outcome := "committed"
	if err != nil {
		outcome = "commit_error"
	}
	tx.finish(outcome, err)
	return err
}

func (tx *transaction) Rollback() error {
	err := tx.Tx.Rollback()
	outcome := "rolled_back"
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		outcome = "rollback_error"
	}
	tx.finish(outcome, err)
	return err
}

func (tx *transaction) finish(outcome string, err error) {
	if !tx.done {
		tx.done = true
		tx.store.logTransaction(tx.ctx, tx.operation, tx.start, outcome, err)
	}
}

func (s *Store) logTransaction(ctx context.Context, operation string, start time.Time, outcome string, err error) {
	if s.Logger == nil {
		return
	}
	level := slog.LevelInfo
	if outcome == "begin_error" || outcome == "commit_error" || outcome == "rollback_error" {
		// Only a canceled operation makes cancellation/ErrTxDone expected.
		if ctx.Err() != context.Canceled || (!errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone)) {
			level = slog.LevelError
		}
	}
	var pg *pgconn.PgError
	code := ""
	if errors.As(err, &pg) {
		code = pg.Code
	}
	stats := s.DB.Stats()
	s.Logger.Log(context.Background(), level, "database transaction",
		"engine", s.Engine, "operation", operation, "outcome", outcome, "sqlstate", code,
		"duration", time.Since(start), "pool_in_use", stats.InUse,
		"pool_wait_count", stats.WaitCount, "pool_wait_duration", stats.WaitDuration)
}
