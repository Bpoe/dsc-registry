namespace Dscregd.Reports;

public record MachineConfigurationStatus(
    string Name,
    string? DesiredRevision,
    string RevisionState,
    string Outcome,
    ReportMetadata? LatestReport,
    ReportMetadata? LatestReceivedReport);