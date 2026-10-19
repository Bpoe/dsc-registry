package database

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	Created bool
}

type ConfigurationStatus struct {
	report.Summary
	HasReport bool   `json:"hasReport"`
	ReportID  string `json:"reportId"`
}

const reportColumns = `id,machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,operation,input_hash,error_kind`

func (m *ReportMetadata) fields() []any {
	return []any{&m.ID, &m.MachineID, &m.ConfigurationName, &m.ConfigurationRevision,
		&m.ReceivedAt, &m.StartedAt, &m.FinishedAt, &m.Outcome, &m.Operation, &m.InputHash, &m.ErrorKind}
}

// PublishReport serializes acceptance using the machine row. The file must be
// synced and closed before commit makes its immutable reference visible.
func (s *Store) PublishReport(ctx context.Context, id, name string, upload report.Upload,
	write func() (string, error), read func(string) ([]byte, error)) (Publication, error) {
	tx, err := s.begin(ctx, "publish_report")
	if err != nil {
		return Publication{}, err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx.Tx, apikey.Machine, id); err != nil {
		return Publication{}, err
	}
	digest := sha256.Sum256(upload.Result)
	var existing ReportMetadata
	var path string
	var storedDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path,result_sha256 FROM reports
		WHERE machine_id = $1 AND name = $2 AND upload_id = $3`, id, name, upload.ID).
		Scan(append(existing.fields(), &path, &storedDigest)...)
	if err == nil {
		if existing.ConfigurationRevision != upload.ConfigurationRevision {
			return Publication{}, ErrUploadConflict
		}
		// Pre-history records have no digest. Verify their original bytes on
		// the first retry, then persist the digest for filesystem-free retries.
		backfillDigest := storedDigest == nil
		if backfillDigest {
			content, err := read(path)
			if err != nil {
				return Publication{}, err
			}
			oldDigest := sha256.Sum256(content)
			storedDigest = oldDigest[:]
		}
		if !bytes.Equal(storedDigest, digest[:]) {
			return Publication{}, ErrUploadConflict
		}
		if backfillDigest {
			if _, err := tx.ExecContext(ctx, `UPDATE reports SET result_sha256 = $1
				WHERE id = $2 AND result_sha256 IS NULL`, digest[:], existing.ID); err != nil {
				return Publication{}, err
			}
		}
		return Publication{Report: existing}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) + 1 FROM reports WHERE machine_id = $1`, id).
		Scan(&sequence); err != nil {
		return Publication{}, err
	}
	path, err = write()
	if err != nil {
		return Publication{}, err
	}
	metadata := ReportMetadata{Summary: upload.Summary, ID: uuid.NewString(), MachineID: id, ConfigurationName: name}
	metadata.ReceivedAt = timestamp()
	_, err = tx.ExecContext(ctx, `INSERT INTO reports
		(id,machine_id,sequence,name,configuration_revision,received_at,started_at,finished_at,outcome,
		 operation,input_hash,error_kind,report_path,upload_id,result_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		metadata.ID, id, sequence, name, upload.ConfigurationRevision, metadata.ReceivedAt, upload.StartedAt,
		upload.FinishedAt, upload.Outcome, upload.Operation, upload.InputHash, upload.ErrorKind, path, upload.ID, digest[:])
	if err != nil {
		return Publication{}, err
	}
	// An ambiguous commit acknowledgement must never delete a committed file.
	// Unreferenced files are reclaimed only by bounded offline maintenance.
	if err := tx.Commit(); err != nil {
		return Publication{}, err
	}
	return Publication{Report: metadata, Created: true}, nil
}

func (s *Store) withMachineReports(ctx context.Context, id string, read func(*sql.Tx) error) error {
	tx, err := s.begin(ctx, "read_report_status")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx.Tx, apikey.Machine, id); err != nil {
		return err
	}
	if err := read(tx.Tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListReports(ctx context.Context, id, cursor string, limit int) ([]ReportMetadata, error) {
	items := make([]ReportMetadata, 0)
	err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
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
			WHERE machine_id = $1 AND ($2 = 0 OR sequence < $2) ORDER BY sequence DESC LIMIT $3`, id, before, limit)
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

// GetReport keeps machine deletion serialized with reading the original file.
// A nil read callback retrieves metadata without touching the filesystem.
func (s *Store) GetReport(ctx context.Context, id, reportID string, read func(string) error) (ReportMetadata, error) {
	var item ReportMetadata
	err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
		var path string
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports
			WHERE machine_id = $1 AND id = $2`, id, reportID).Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		if err != nil {
			return err
		}
		if read != nil {
			return read(path)
		}
		return nil
	})
	return item, err
}

func (s *Store) latestConfigurationReport(ctx context.Context, id, name string, read func(string) error) (ReportMetadata, error) {
	var item ReportMetadata
	err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
		var revision string
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM configurations WHERE machine_id = $1 AND name = $2`, id, name).
			Scan(&revision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConfigurationNotFound
			}
			return err
		}
		var path string
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports
			WHERE machine_id = $1 AND name = $2 ORDER BY sequence DESC LIMIT 1`, id, name).
			Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		if err != nil {
			return err
		}
		if read != nil {
			return read(path)
		}
		return nil
	})
	return item, err
}

func (s *Store) GetConfigurationStatus(ctx context.Context, id, name string) (ConfigurationStatus, error) {
	item, err := s.latestConfigurationReport(ctx, id, name, nil)
	return ConfigurationStatus{Summary: item.Summary, HasReport: err == nil, ReportID: item.ID}, err
}

func (s *Store) ReadReport(ctx context.Context, id, name string, read func(string) error) error {
	_, err := s.latestConfigurationReport(ctx, id, name, read)
	return err
}

func (s *Store) RetireReport(ctx context.Context, path string, remove func(string) error) error {
	var referenced bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM reports WHERE report_path = $1)`, path).
		Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return nil
	}
	return remove(path)
}
