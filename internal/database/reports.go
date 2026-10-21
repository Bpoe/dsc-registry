package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"

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
	Created      bool
	CleanupPaths []string
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

var errReportChanged = errors.New("report changed during publication")

// PublishReport performs file I/O outside transactions. The caller must remove
// CleanupPaths after return, including on error; an uncertain commit never
// returns the potentially published file for cleanup.
func (s *Store) PublishReport(ctx context.Context, id, name string, upload report.Upload,
	write func() (string, error), read func(string) ([]byte, error)) (publication Publication, err error) {
	var unused []string
	defer func() { publication.CleanupPaths = append(publication.CleanupPaths, unused...) }()
	digest := sha256.Sum256(upload.Result)
	for {
		var existingID, revision, existingPath string
		var storedDigest []byte
		err := s.DB.QueryRowContext(ctx, `SELECT id,configuration_revision,report_path,result_sha256 FROM reports r
			WHERE machine_id = $1 AND name = $2 AND upload_id = $3
			AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`,
			id, name, upload.ID).Scan(&existingID, &revision, &existingPath, &storedDigest)
		path := ""
		switch {
		case err == nil:
			if revision != upload.ConfigurationRevision {
				return Publication{}, ErrUploadConflict
			}
			if storedDigest == nil {
				content, err := read(existingPath)
				if err != nil {
					return Publication{}, err
				}
				oldDigest := sha256.Sum256(content)
				storedDigest = oldDigest[:]
			}
			if !bytes.Equal(storedDigest, digest[:]) {
				return Publication{}, ErrUploadConflict
			}
		case errors.Is(err, sql.ErrNoRows):
			path, err = write()
			if err != nil {
				return Publication{}, err
			}
		default:
			return Publication{}, err
		}
		publication, keepFile, err := s.publishReport(ctx, id, name, upload, path, digest[:], existingID)
		if path != "" && !keepFile {
			unused = append(unused, path)
		}
		if errors.Is(err, errReportChanged) {
			// A retry's report was replaced after the preliminary lookup.
			// Recheck it without holding a transaction during file I/O.
			continue
		}
		return publication, err
	}
}

func (s *Store) publishReport(ctx context.Context, id, name string, upload report.Upload,
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
				return Publication{}, false, errReportChanged
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
		return Publication{}, false, errReportChanged
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
	return Publication{Report: metadata, Created: true, CleanupPaths: previous}, true, nil
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

// A nil read callback retrieves metadata without touching the filesystem.
func (s *Store) GetReport(ctx context.Context, id, reportID string, read func(string) error) (ReportMetadata, error) {
	var item ReportMetadata
	var path string
	err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT `+reportColumns+`,report_path FROM reports r
			WHERE machine_id = $1 AND id = $2
			AND sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = r.machine_id AND name = r.name)`, id, reportID).
			Scan(append(item.fields(), &path)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportNotFound
		}
		return err
	})
	if err == nil && read != nil {
		err = read(path)
		if errors.Is(err, os.ErrNotExist) {
			if _, lookupErr := s.GetReport(ctx, id, reportID, nil); lookupErr != nil {
				return item, lookupErr
			}
		}
	}
	return item, err
}

func (s *Store) latestConfigurationReport(ctx context.Context, id, name string, read func(string) error) (ReportMetadata, error) {
	var missingID string
	var missingErr error
	for {
		var item ReportMetadata
		var path string
		err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
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
		if err == nil && read != nil {
			if item.ID == missingID {
				return item, missingErr
			}
			err = read(path)
			if errors.Is(err, os.ErrNotExist) {
				missingID, missingErr = item.ID, err
				continue
			}
		}
		return item, err
	}
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
