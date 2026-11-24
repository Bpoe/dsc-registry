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
	Report  ReportMetadata
	Outcome MutationOutcome
}

const reportColumns = `id,machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,operation,input_hash,error_kind`

func (m *ReportMetadata) fields() []any {
	return []any{&m.ID, &m.MachineID, &m.ConfigurationName, &m.ConfigurationRevision,
		&m.ReceivedAt, &m.StartedAt, &m.FinishedAt, &m.Outcome, &m.Operation, &m.InputHash, &m.ErrorKind}
}

// ReportCommit carries the result of the locked mutation. LegacyPath requires
// verification outside the transaction. KeepFile also covers ambiguous commits.
type ReportCommit struct {
	Publication
	LegacyPath string
	KeepFile   bool
}

// CommitReport applies a new receipt or verifies an idempotent retry. An empty
// path permits retry verification only. verifiedID proves a legacy file was
// hashed outside the transaction and still belongs to the same receipt.
func (s *Store) CommitReport(ctx context.Context, id, name string, upload report.Upload,
	path string, digest []byte, verifiedID string) (ReportCommit, error) {
	tx, err := s.begin(ctx, "publish_report")
	if err != nil {
		return ReportCommit{}, err
	}
	defer tx.Rollback()
	// Keep the lock in a separate statement: after waiting for a PostgreSQL
	// writer, the mutation needs a fresh READ COMMITTED snapshot for MAX.
	if err := s.lockOwner(ctx, tx.Tx, apikey.Machine, id); err != nil {
		return ReportCommit{}, err
	}

	if path != "" {
		metadata := ReportMetadata{Summary: upload.Summary, ID: uuid.NewString(), MachineID: id, ConfigurationName: name}
		metadata.ReceivedAt = timestamp()
		var publishedID string
		err = tx.QueryRowContext(ctx, `UPDATE reports SET
            sequence = (SELECT COALESCE(MAX(sequence),0) + 1 FROM reports WHERE machine_id = $1),
            id = $3, configuration_revision = $4, received_at = $5, started_at = $6,
            finished_at = $7, outcome = $8, operation = $9, input_hash = $10, error_kind = $11,
            report_path = $12, upload_id = $13, result_sha256 = $14
            WHERE machine_id = $1 AND name = $2 AND upload_id <> $13
            RETURNING id`, id, name, metadata.ID, upload.ConfigurationRevision, metadata.ReceivedAt,
			upload.StartedAt, upload.FinishedAt, upload.Outcome, upload.Operation, upload.InputHash,
			upload.ErrorKind, path, upload.ID, digest).Scan(&publishedID)
		if err == nil {
			return commitReceipt(tx, metadata, MutationUpdated)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ReportCommit{}, err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO reports
            (id,machine_id,sequence,name,configuration_revision,received_at,started_at,finished_at,outcome,
             operation,input_hash,error_kind,report_path,upload_id,result_sha256)
            VALUES ($1,$2,(SELECT COALESCE(MAX(sequence),0) + 1 FROM reports WHERE machine_id = $2),
                    $3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
            ON CONFLICT(machine_id,name) DO NOTHING RETURNING id`,
			metadata.ID, id, name, upload.ConfigurationRevision, metadata.ReceivedAt, upload.StartedAt,
			upload.FinishedAt, upload.Outcome, upload.Operation, upload.InputHash, upload.ErrorKind, path, upload.ID, digest).
			Scan(&publishedID)
		if err == nil {
			return commitReceipt(tx, metadata, MutationCreated)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ReportCommit{}, err
		}
	}

	var existing ReportMetadata
	var existingPath string
	var storedDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path,result_sha256 FROM reports
        WHERE machine_id = $1 AND name = $2 AND upload_id = $3`, id, name, upload.ID).
		Scan(append(existing.fields(), &existingPath, &storedDigest)...)
	if err == nil {
		if existing.ConfigurationRevision != upload.ConfigurationRevision ||
			(storedDigest != nil && !bytes.Equal(storedDigest, digest)) {
			return ReportCommit{}, ErrUploadConflict
		}
		result := ReportCommit{Publication: Publication{Report: existing, Outcome: MutationUnchanged}}
		if storedDigest != nil {
			return result, nil
		}
		if existing.ID != verifiedID {
			result.LegacyPath = existingPath
			return result, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET result_sha256 = $1 WHERE id = $2`, digest, existing.ID); err != nil {
			return ReportCommit{}, err
		}
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReportCommit{}, err
	}
	return ReportCommit{}, ErrReportNotFound
}

func commitReceipt(tx *transaction, metadata ReportMetadata, outcome MutationOutcome) (ReportCommit, error) {
	if err := tx.Commit(); err != nil {
		return ReportCommit{KeepFile: true}, err
	}
	return ReportCommit{Publication: Publication{Report: metadata, Outcome: outcome}, KeepFile: true}, nil
}

func (s *Store) ListReports(ctx context.Context, id, cursor string, limit int) ([]ReportMetadata, error) {
	items := make([]ReportMetadata, 0)
	err := s.withOwner(ctx, "read_report_status", apikey.Machine, id, func(tx *sql.Tx) error {
		var before int64
		if cursor != "" {
			err := tx.QueryRowContext(ctx, `SELECT sequence FROM reports WHERE machine_id = $1 AND id = $2`, id, cursor).Scan(&before)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrReportCursor
			}
			if err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+reportColumns+` FROM reports
			WHERE machine_id = $1 AND ($2 = 0 OR sequence < $2)
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
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports
			WHERE machine_id = $1 AND id = $2`, id, reportID).
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
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports
			WHERE machine_id = $1 AND name = $2 AND configuration_revision = $3`, id, name, revision).
			Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		return err
	})
	return item, path, err
}
