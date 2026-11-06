package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/google/uuid"
)

var (
	ErrUploadConflict = errors.New("upload ID already used for different report")
	ErrReportNotFound = errors.New("report not found")
	ErrReportCursor   = errors.New("report cursor not found for machine")
)

type ReportMetadata struct {
	report.Summary
	ID                string `json:"id"`
	MachineID         string `json:"machineId"`
	ConfigurationName string `json:"configurationName"`
}

type Publication struct {
	Report       ReportMetadata
	Outcome      MutationOutcome
	CleanupPaths []string
}

const reportColumns = `id,machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,operation,input_hash,error_kind`

func (m *ReportMetadata) fields() []any {
	return []any{&m.ID, &m.MachineID, &m.ConfigurationName, &m.ConfigurationRevision,
		&m.ReceivedAt, &m.StartedAt, &m.FinishedAt, &m.Outcome, &m.Operation, &m.InputHash, &m.ErrorKind}
}

var ErrReportChanged = errors.New("report changed during publication")

// FindReportUpload reads a retained upload without acquiring a transaction.
func (s *Store) FindReportUpload(ctx context.Context, id, name, uploadID string) (existingID, revision, path string, digest []byte, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT id,configuration_revision,report_path,result_sha256 FROM reports r
 WHERE machine_id = $1 AND name = $2 AND upload_id = $3
 AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`, id, name, uploadID).Scan(&existingID, &revision, &path, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrReportNotFound
	}
	return
}

// CommitReport atomically verifies a retry or replaces retained metadata under
// the machine lock. keepFile is true on success or an ambiguous commit outcome;
// callers must retain the candidate in either case. No file I/O occurs here.
func (s *Store) CommitReport(ctx context.Context, id, name string, upload report.Upload,
	path string, digest []byte, verifiedID string) (Publication, bool, error) {
	tx, err := s.begin(ctx, "publish_report")
	if err != nil {
		return Publication{}, false, err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx.Tx, apikey.Machine, id); err != nil {
		return Publication{}, false, err
	}
	var existing ReportMetadata
	var storedDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,result_sha256 FROM reports r
		WHERE machine_id = $1 AND name = $2 AND upload_id = $3
		AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`, id, name, upload.ID).
		Scan(append(existing.fields(), &storedDigest)...)
	if err == nil {
		if existing.ConfigurationRevision != upload.ConfigurationRevision {
			return Publication{}, false, ErrUploadConflict
		}
		backfillDigest := storedDigest == nil
		if backfillDigest {
			if existing.ID != verifiedID {
				return Publication{}, false, ErrReportChanged
			}
			storedDigest = digest
		}
		if !bytes.Equal(storedDigest, digest) {
			return Publication{}, false, ErrUploadConflict
		}
		if backfillDigest {
			if _, err := tx.ExecContext(ctx, `UPDATE reports SET result_sha256 = $1
				WHERE id = $2 AND result_sha256 IS NULL`, digest, existing.ID); err != nil {
				return Publication{}, false, err
			}
		}
		return Publication{Report: existing}, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, false, err
	}
	if path == "" {
		return Publication{}, false, ErrReportChanged
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) + 1 FROM reports WHERE machine_id = $1`, id).
		Scan(&sequence); err != nil {
		return Publication{}, false, err
	}
	previous, err := reportPaths(ctx, tx.Tx, id, name)
	if err != nil {
		return Publication{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE machine_id = $1 AND name = $2`, id, name); err != nil {
		return Publication{}, false, err
	}
	metadata := ReportMetadata{Summary: upload.Summary, ID: uuid.NewString(), MachineID: id, ConfigurationName: name}
	metadata.ReceivedAt = timestamp()
	_, err = tx.ExecContext(ctx, `INSERT INTO reports
		(id,machine_id,sequence,name,configuration_revision,received_at,started_at,finished_at,outcome,
		 operation,input_hash,error_kind,report_path,upload_id,result_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		metadata.ID, id, sequence, name, upload.ConfigurationRevision, metadata.ReceivedAt, upload.StartedAt,
		upload.FinishedAt, upload.Outcome, upload.Operation, upload.InputHash, upload.ErrorKind, path, upload.ID, digest)
	if err != nil {
		return Publication{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Publication{}, true, err
	}
	outcome := MutationCreated
	if len(previous) != 0 {
		outcome = MutationUpdated
	}
	return Publication{Report: metadata, Outcome: outcome, CleanupPaths: previous}, true, nil
}

func reportPaths(ctx context.Context, tx *sql.Tx, id, name string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT report_path FROM reports
		WHERE machine_id = $1 AND ($2 = '' OR name = $2)`, id, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (s *Store) ListReports(ctx context.Context, id, cursor string, limit int) ([]ReportMetadata, error) {
	items := make([]ReportMetadata, 0)
	err := s.withOwner(ctx, "read_report_status", apikey.Machine, id, func(tx *sql.Tx) error {
		var before int64
		if cursor != "" {
			err := tx.QueryRowContext(ctx, `SELECT sequence FROM reports r WHERE machine_id = $1 AND id = $2
				AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`, id, cursor).Scan(&before)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrReportCursor
			}
			if err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+reportColumns+` FROM reports r
			WHERE machine_id = $1 AND ($2 = 0 OR sequence < $2)
			AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)
			ORDER BY sequence DESC LIMIT $3`, id, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item ReportMetadata
			if err := rows.Scan(item.fields()...); err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return items, err
}

// GetReport returns a retained reference; callers read content after the transaction.
func (s *Store) GetReport(ctx context.Context, id, reportID string) (ReportMetadata, string, error) {
	var item ReportMetadata
	var path string
	err := s.withOwner(ctx, "read_report_status", apikey.Machine, id, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports r
			WHERE machine_id = $1 AND id = $2
			AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`, id, reportID).
			Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		return err
	})
	return item, path, err
}

func (s *Store) LatestConfigurationReport(ctx context.Context, id, name string) (ReportMetadata, string, error) {
	var item ReportMetadata
	var path string
	err := s.withOwner(ctx, "read_report_status", apikey.Machine, id, func(tx *sql.Tx) error {
		var revision string
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM configurations WHERE machine_id = $1 AND name = $2`, id, name).
			Scan(&revision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConfigurationNotFound
			}
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports r
			WHERE machine_id = $1 AND name = $2 AND configuration_revision = $3
			AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)
			ORDER BY sequence DESC LIMIT 1`, id, name, revision).
			Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		return err
	})
	return item, path, err
}
