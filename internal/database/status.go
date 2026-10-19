package database

import (
	"context"
	"database/sql"
)

type MachineConfigurationStatus struct {
	Name            string          `json:"name"`
	DesiredRevision *string         `json:"desiredRevision"`
	RevisionState   string          `json:"revisionState"`
	Outcome         string          `json:"outcome"`
	LatestReport    *ReportMetadata `json:"latestReport"`
}

type MachineStatus struct {
	MachineID             string                       `json:"machineId"`
	ConfigurationRevision int64                        `json:"configurationRevision"`
	HasReport             bool                         `json:"hasReport"`
	LastReportReceivedAt  *string                      `json:"lastReportReceivedAt"`
	LastExecutionAt       *string                      `json:"lastExecutionAt"`
	LatestReport          *ReportMetadata              `json:"latestReport"`
	Outcome               string                       `json:"outcome"`
	HasStaleReports       bool                         `json:"hasStaleReports"`
	Configurations        []MachineConfigurationStatus `json:"configurations"`
}

func (s *Store) GetMachineStatus(ctx context.Context, id string) (MachineStatus, error) {
	status := MachineStatus{MachineID: id, Outcome: "never_reported", Configurations: make([]MachineConfigurationStatus, 0)}
	err := s.withMachineReports(ctx, id, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT configuration_revision FROM machines WHERE id = $1`, id).
			Scan(&status.ConfigurationRevision); err != nil {
			return err
		}
		// The machine lock gives all queries one stable view even under
		// PostgreSQL READ COMMITTED. No mutable status projection is stored.
		rows, err := tx.QueryContext(ctx, `SELECT names.name,c.revision,
			r.id,r.machine_id,r.name,r.configuration_revision,r.received_at,r.started_at,r.finished_at,
			r.outcome,r.operation,r.input_hash,r.error_kind,r.sequence
			FROM (SELECT name FROM configurations WHERE machine_id = $1
			      UNION SELECT name FROM reports WHERE machine_id = $1) names
			LEFT JOIN configurations c ON c.machine_id = $1 AND c.name = names.name
			LEFT JOIN reports r ON r.machine_id = $1 AND r.name = names.name
				AND r.sequence = (SELECT MAX(sequence) FROM reports WHERE machine_id = $1 AND name = names.name)
			ORDER BY names.name`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		var latestSequence int64
		currentCount := 0
		aggregate := "succeeded"
		for rows.Next() {
			item := MachineConfigurationStatus{RevisionState: "unreported", Outcome: "never_reported"}
			var reportID, machineID, name, revision, received, started, finished, outcome, operation, hash, errorKind sql.NullString
			var sequence sql.NullInt64
			if err := rows.Scan(&item.Name, &item.DesiredRevision, &reportID, &machineID, &name, &revision,
				&received, &started, &finished, &outcome, &operation, &hash, &errorKind, &sequence); err != nil {
				return err
			}
			if reportID.Valid {
				metadata := &ReportMetadata{ID: reportID.String, MachineID: machineID.String, ConfigurationName: name.String}
				metadata.ConfigurationRevision, metadata.ReceivedAt = revision.String, received.String
				metadata.StartedAt, metadata.FinishedAt, metadata.Outcome = started.String, finished.String, outcome.String
				metadata.Operation, metadata.InputHash, metadata.ErrorKind = nullableString(operation), nullableString(hash), nullableString(errorKind)
				item.LatestReport, item.Outcome = metadata, metadata.Outcome
				switch {
				case item.DesiredRevision == nil:
					item.RevisionState = "removed"
				case *item.DesiredRevision != metadata.ConfigurationRevision:
					item.RevisionState = "stale"
					status.HasStaleReports = true
				default:
					item.RevisionState = "current"
				}
				if sequence.Int64 > latestSequence {
					latestSequence = sequence.Int64
					status.LatestReport = metadata
					status.LastReportReceivedAt, status.LastExecutionAt = &metadata.ReceivedAt, &metadata.FinishedAt
					status.HasReport = true
				}
			}
			if item.DesiredRevision != nil {
				currentCount++
				switch {
				case item.Outcome == "failed":
					aggregate = "failed"
				case item.Outcome == "canceled" && aggregate != "failed":
					aggregate = "canceled"
				case item.LatestReport == nil && aggregate == "succeeded":
					aggregate = "unknown"
				}
			}
			status.Configurations = append(status.Configurations, item)
		}
		if status.HasReport {
			status.Outcome = aggregate
			if currentCount == 0 {
				status.Outcome = "unknown"
			}
		}
		return rows.Err()
	})
	return status, err
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
