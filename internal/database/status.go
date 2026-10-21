package database

import (
	"context"
	"database/sql"
)

type MachineConfigurationStatus struct {
	Name                 string          `json:"name"`
	DesiredRevision      *string         `json:"desiredRevision"`
	RevisionState        string          `json:"revisionState"`
	Outcome              string          `json:"outcome"`
	LatestReport         *ReportMetadata `json:"latestReport"`
	LatestReceivedReport *ReportMetadata `json:"latestReceivedReport"`
}

type MachineStatus struct {
	MachineID             string                       `json:"machineId"`
	ConfigurationRevision int64                        `json:"configurationRevision"`
	HasReport             bool                         `json:"hasReport"`
	LastReportReceivedAt  *string                      `json:"lastReportReceivedAt"`
	LastExecutionAt       *string                      `json:"lastExecutionAt"`
	LatestReport          *ReportMetadata              `json:"latestReport"`
	LatestReceivedReport  *ReportMetadata              `json:"latestReceivedReport"`
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
			ORDER BY names.name,r.sequence`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		var latestReceivedSequence, latestCurrentSequence int64
		for rows.Next() {
			var configurationName string
			var desiredRevision *string
			var reportID, machineID, name, revision, received, started, finished, outcome, operation, hash, errorKind sql.NullString
			var sequence sql.NullInt64
			if err := rows.Scan(&configurationName, &desiredRevision, &reportID, &machineID, &name, &revision,
				&received, &started, &finished, &outcome, &operation, &hash, &errorKind, &sequence); err != nil {
				return err
			}
			if len(status.Configurations) == 0 || status.Configurations[len(status.Configurations)-1].Name != configurationName {
				status.Configurations = append(status.Configurations, MachineConfigurationStatus{
					Name: configurationName, DesiredRevision: desiredRevision,
					RevisionState: "unreported", Outcome: "never_reported",
				})
			}
			item := &status.Configurations[len(status.Configurations)-1]
			if reportID.Valid {
				metadata := &ReportMetadata{ID: reportID.String, MachineID: machineID.String, ConfigurationName: name.String}
				metadata.ConfigurationRevision, metadata.ReceivedAt = revision.String, received.String
				metadata.StartedAt, metadata.FinishedAt, metadata.Outcome = started.String, finished.String, outcome.String
				metadata.Operation, metadata.InputHash, metadata.ErrorKind = nullableString(operation), nullableString(hash), nullableString(errorKind)
				item.LatestReceivedReport = metadata
				if sequence.Int64 > latestReceivedSequence {
					latestReceivedSequence = sequence.Int64
					status.LatestReceivedReport = metadata
					status.LastReportReceivedAt = &metadata.ReceivedAt
					status.HasReport = true
				}
				if item.DesiredRevision != nil && *item.DesiredRevision == metadata.ConfigurationRevision {
					item.LatestReport = metadata
					if sequence.Int64 > latestCurrentSequence {
						latestCurrentSequence = sequence.Int64
						status.LatestReport = metadata
						status.LastExecutionAt = &metadata.FinishedAt
					}
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		currentCount := 0
		aggregate := "succeeded"
		for i := range status.Configurations {
			item := &status.Configurations[i]
			switch {
			case item.DesiredRevision == nil:
				item.RevisionState, item.Outcome = "removed", "unknown"
			case item.LatestReport != nil:
				item.RevisionState, item.Outcome = "current", item.LatestReport.Outcome
			case item.LatestReceivedReport != nil:
				item.RevisionState, item.Outcome = "stale", "unknown"
				status.HasStaleReports = true
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
		}
		if status.HasReport {
			status.Outcome = aggregate
			if currentCount == 0 {
				status.Outcome = "unknown"
			}
		}
		return nil
	})
	return status, err
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
