package database

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type ReportCleanup struct {
	Path     string
	Attempts int
}

// EnqueueReportCleanup records a file that was created but will not be
// published. Retired metadata paths are enqueued by the database triggers.
func (s *Store) EnqueueReportCleanup(ctx context.Context, path string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO report_file_cleanup(path) VALUES ($1)
        ON CONFLICT(path) DO NOTHING`, path)
	return err
}

func (s *Store) cleanupClock() string {
	if s.Engine == "postgres" {
		return "floor(extract(epoch FROM statement_timestamp()))::bigint"
	}
	return "unixepoch()"
}

// ClaimReportCleanup releases its connection before returning work. PostgreSQL
// workers skip each other's locked rows and SQLite serializes the single UPDATE.
// Published files always use fresh keys. A retired or abandoned key cannot be
// published again between the reference check and the unlink.
func (s *Store) ClaimReportCleanup(ctx context.Context, token string, limit int, lease time.Duration) ([]ReportCleanup, error) {
	locking := ""
	if s.Engine == "postgres" {
		locking = " FOR UPDATE SKIP LOCKED"
	}
	now := s.cleanupClock()
	rows, err := s.DB.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
            SELECT pending.path FROM report_file_cleanup pending
            WHERE pending.available_at <= `+now+`
              AND NOT EXISTS (SELECT 1 FROM reports WHERE report_path = pending.path)
            ORDER BY pending.available_at, pending.path LIMIT $2`+locking+`
        )
        UPDATE report_file_cleanup
        SET claim_token = $1, available_at = `+now+` + $3, attempts = attempts + 1
        WHERE path IN (SELECT path FROM candidates)
        RETURNING path, attempts`, token, limit, int64(lease/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var work []ReportCleanup
	for rows.Next() {
		var item ReportCleanup
		if err := rows.Scan(&item.Path, &item.Attempts); err != nil {
			return nil, err
		}
		work = append(work, item)
	}
	return work, rows.Err()
}

// A stale worker's token cannot acknowledge or reschedule a reclaimed item.
func (s *Store) CompleteReportCleanup(ctx context.Context, token string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := []any{token}
	params := make([]string, 0, len(paths))
	for _, path := range paths {
		args = append(args, path)
		params = append(params, "$"+strconv.Itoa(len(args)))
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM report_file_cleanup
        WHERE claim_token = $1 AND path IN (`+strings.Join(params, ",")+`)`, args...)
	return err
}

func (s *Store) RetryReportCleanup(ctx context.Context, token, path string, delay time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE report_file_cleanup
        SET claim_token = NULL, available_at = `+s.cleanupClock()+` + $3
        WHERE claim_token = $1 AND path = $2`, token, path, int64(delay/time.Second))
	return err
}

func (s *Store) ReportCleanupBacklog(ctx context.Context) (int64, float64, error) {
	var count int64
	var age int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(`+s.cleanupClock()+` - MIN(enqueued_at), 0)
        FROM report_file_cleanup`).Scan(&count, &age)
	return count, float64(max(age, 0)), err
}
