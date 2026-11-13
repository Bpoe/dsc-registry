package service

import (
	"context"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
)

type MachineConfigurationStatus struct {
	Name                 string                   `json:"name"`
	DesiredRevision      *string                  `json:"desiredRevision"`
	RevisionState        string                   `json:"revisionState"`
	Outcome              string                   `json:"outcome"`
	LatestReport         *database.ReportMetadata `json:"latestReport"`
	LatestReceivedReport *database.ReportMetadata `json:"latestReceivedReport"`
}

type MachineStatus struct {
	MachineID             string                       `json:"machineId"`
	ConfigurationRevision int64                        `json:"configurationRevision"`
	HasReport             bool                         `json:"hasReport"`
	LastReportReceivedAt  *string                      `json:"lastReportReceivedAt"`
	LastExecutionAt       *string                      `json:"lastExecutionAt"`
	LatestReport          *database.ReportMetadata     `json:"latestReport"`
	LatestReceivedReport  *database.ReportMetadata     `json:"latestReceivedReport"`
	Outcome               string                       `json:"outcome"`
	HasStaleReports       bool                         `json:"hasStaleReports"`
	Configurations        []MachineConfigurationStatus `json:"configurations"`
}

type ConfigurationStatus struct {
	report.Summary
	HasReport bool   `json:"hasReport"`
	ReportID  string `json:"reportId"`
}

func (s *Service) GetMachineStatus(ctx context.Context, id string) (MachineStatus, error) {
	status := MachineStatus{MachineID: id, Outcome: "never_reported", Configurations: make([]MachineConfigurationStatus, 0)}
	snapshot, err := s.db.MachineReportSnapshot(ctx, id)
	if err != nil {
		return status, err
	}
	status.ConfigurationRevision = snapshot.ConfigurationRevision
	var latestReceivedSequence, latestCurrentSequence int64
	for _, row := range snapshot.Configurations {
		item := MachineConfigurationStatus{Name: row.Name, DesiredRevision: row.DesiredRevision, RevisionState: "unreported", Outcome: "never_reported", LatestReceivedReport: row.Report}
		if metadata := row.Report; metadata != nil {
			if row.Sequence > latestReceivedSequence {
				latestReceivedSequence = row.Sequence
				status.LatestReceivedReport = metadata
				status.LastReportReceivedAt = &metadata.ReceivedAt
				status.HasReport = true
			}
			if row.DesiredRevision != nil && *row.DesiredRevision == metadata.ConfigurationRevision {
				item.LatestReport = metadata
				if row.Sequence > latestCurrentSequence {
					latestCurrentSequence = row.Sequence
					status.LatestReport = metadata
					status.LastExecutionAt = &metadata.FinishedAt
				}
			}
		}
		status.Configurations = append(status.Configurations, item)
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
	return status, nil
}

func (s *Service) GetConfigurationStatus(ctx context.Context, id, name string) (ConfigurationStatus, error) {
	item, _, err := s.db.LatestConfigurationReport(ctx, id, name)
	return ConfigurationStatus{Summary: item.Summary, HasReport: err == nil, ReportID: item.ID}, err
}
