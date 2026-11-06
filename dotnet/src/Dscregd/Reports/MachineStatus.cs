namespace Dscregd.Reports;

public record MachineStatus(
    string MachineId,
    long ConfigurationRevision,
    bool HasReport,
    string? LastReportReceivedAt,
    string? LastExecutionAt,
    ReportMetadata? LatestReport,
    ReportMetadata? LatestReceivedReport,
    string Outcome,
    bool HasStaleReports,
    MachineConfigurationStatus[] Configurations);