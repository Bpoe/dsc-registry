package database

import (
	"context"
	"database/sql"
	"github.com/Bpoe/dsc-registry/internal/apikey"
)

// MachineReportSnapshot is a consistent SQL view of desired revisions and
// retained receipts. The service derives execution status from this data.
type MachineReportSnapshot struct {
	ConfigurationRevision int64
	Configurations        []ConfigurationReport
}
type ConfigurationReport struct {
	Name            string
	DesiredRevision *string
	Report          *ReportMetadata
	Sequence        int64
}

func (s *Store) MachineReportSnapshot(ctx context.Context, id string) (MachineReportSnapshot, error) {
	var snapshot MachineReportSnapshot
	err := s.withOwner(ctx, "read_report_status", apikey.Machine, id, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT configuration_revision FROM machines WHERE id = $1`, id).Scan(&snapshot.ConfigurationRevision); err != nil {
			return err
		}
		// The machine lock keeps both queries consistent under READ COMMITTED.
		rows, err := tx.QueryContext(ctx, `SELECT names.name,c.revision,
			r.id,r.machine_id,r.name,r.configuration_revision,r.received_at,r.started_at,r.finished_at,
			r.outcome,r.operation,r.input_hash,r.error_kind,r.sequence
			FROM (SELECT name FROM configurations WHERE machine_id = $1
			      UNION SELECT name FROM reports WHERE machine_id = $1) names
			LEFT JOIN configurations c ON c.machine_id = $1 AND c.name = names.name
			LEFT JOIN reports r ON r.machine_id = $1 AND r.name = names.name
				AND r.sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = $1 AND name = names.name)
			ORDER BY names.name,r.sequence`, id)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var configurationName string
			var desiredRevision *string
			var reportID, machineID, name, revision, received, started, finished, outcome, operation, hash, errorKind sql.NullString
			var sequence sql.NullInt64
			if err := rows.Scan(&configurationName, &desiredRevision, &reportID, &machineID, &name, &revision,
				&received, &started, &finished, &outcome, &operation, &hash, &errorKind, &sequence); err != nil {
				return err
			}
			item := ConfigurationReport{Name: configurationName, DesiredRevision: desiredRevision, Sequence: sequence.Int64}
			if reportID.Valid {
				metadata := &ReportMetadata{ID: reportID.String, MachineID: machineID.String, ConfigurationName: name.String}
				metadata.ConfigurationRevision, metadata.ReceivedAt = revision.String, received.String
				metadata.StartedAt, metadata.FinishedAt, metadata.Outcome = started.String, finished.String, outcome.String
				metadata.Operation, metadata.InputHash, metadata.ErrorKind = nullableString(operation), nullableString(hash), nullableString(errorKind)
				item.Report = metadata
			}
			snapshot.Configurations = append(snapshot.Configurations, item)
		}
		return rows.Err()
	})
	return snapshot, err
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
