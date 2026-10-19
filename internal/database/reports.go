package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/report"
)

var (
	ErrStaleRevision  = errors.New("stale configuration revision")
	ErrReportNotFound = errors.New("report not found")
)

type ConfigurationStatus struct {
	report.Summary
	HasReport bool `json:"hasReport"`
}

// PublishReport serializes publishers, desired-state mutations and report
// readers using the machine row (SQLite BEGIN IMMEDIATE / PostgreSQL FOR UPDATE).
// The callback completes and closes a unique file before SQL can reference it.
func (s *Store) PublishReport(ctx context.Context, id, name string, upload report.Upload, write func() (string, error)) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, apikey.Machine, id); err != nil {
		return "", err
	}
	revision, err := reportRevision(ctx, tx, id, name)
	if err != nil {
		return "", err
	}
	if revision != upload.ConfigurationRevision {
		return "", ErrStaleRevision
	}
	var previous, lastID, lastRevision string
	err = tx.QueryRowContext(ctx, `SELECT report_path, last_upload_id, configuration_revision FROM configuration_status
		WHERE machine_id = $1 AND name = $2`, id, name).Scan(&previous, &lastID, &lastRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if lastID == upload.ID && lastRevision == revision {
		return "", tx.Commit()
	}
	path, err := write()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO configuration_status
		(machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,operation,input_hash,error_kind,report_path,last_upload_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (machine_id,name) DO UPDATE SET configuration_revision = excluded.configuration_revision,
		received_at = excluded.received_at, started_at = excluded.started_at, finished_at = excluded.finished_at,
		outcome = excluded.outcome, operation = excluded.operation, input_hash = excluded.input_hash,
		error_kind = excluded.error_kind, report_path = excluded.report_path, last_upload_id = excluded.last_upload_id`,
		id, name, upload.ConfigurationRevision, timestamp(), upload.StartedAt, upload.FinishedAt, upload.Outcome,
		upload.Operation, upload.InputHash, upload.ErrorKind, path, upload.ID)
	if err != nil {
		return "", err
	}
	// On any SQL/commit error leave the new file for offline cleanup. A lost
	// commit acknowledgement must never cause deletion of a committed report.
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return previous, nil
}

func (s *Store) GetConfigurationStatus(ctx context.Context, id, name string) (ConfigurationStatus, error) {
	var status ConfigurationStatus
	err := s.withReport(ctx, id, name, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT configuration_revision,received_at,started_at,finished_at,outcome,operation,input_hash,error_kind
			FROM configuration_status WHERE machine_id = $1 AND name = $2`, id, name).
			Scan(&status.ConfigurationRevision, &status.ReceivedAt, &status.StartedAt, &status.FinishedAt,
				&status.Outcome, &status.Operation, &status.InputHash, &status.ErrorKind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		status.HasReport = err == nil
		return err
	})
	return status, err
}

// ReadReport keeps the row lock until the file has been read and closed.
// A publisher cannot retire a file selected by an in-flight reader.
func (s *Store) ReadReport(ctx context.Context, id, name string, read func(string) error) error {
	return s.withReport(ctx, id, name, func(tx *sql.Tx) error {
		var path string
		err := tx.QueryRowContext(ctx, `SELECT report_path FROM configuration_status
			WHERE machine_id = $1 AND name = $2`, id, name).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		if err != nil {
			return err
		}
		return read(path)
	})
}

func (s *Store) withReport(ctx context.Context, id, name string, read func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, apikey.Machine, id); err != nil {
		return err
	}
	if _, err := reportRevision(ctx, tx, id, name); err != nil {
		return err
	}
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func reportRevision(ctx context.Context, tx *sql.Tx, id, name string) (string, error) {
	var revision string
	err := tx.QueryRowContext(ctx, `SELECT revision FROM configurations WHERE machine_id = $1 AND name = $2`, id, name).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConfigurationNotFound
	}
	return revision, err
}

func (s *Store) RetireReport(ctx context.Context, path string, remove func(string) error) error {
	var referenced bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM configuration_status WHERE report_path = $1)`, path).
		Scan(&referenced); err != nil {
		return err
	}
	// Keys are exclusively created, never reused, and only published once.
	// After retirement no future transaction can introduce a reference.
	if referenced {
		return nil
	}
	return remove(path)
}
