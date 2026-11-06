namespace Dscregd.Reports;

using Dscregd.Data;

public record ReportMetadata(
    string Id,
    string MachineId,
    string ConfigurationName,
    string ConfigurationRevision,
    string ReceivedAt,
    string StartedAt,
    string FinishedAt,
    string Outcome,
    string? Operation,
    string? InputHash,
    string? ErrorKind)
{
    public static ReportMetadata FromReport(Report report)
    {
        return new ReportMetadata(report.Id, report.MachineId, report.Name,
            report.ConfigurationRevision, report.ReceivedAt, report.StartedAt, report.FinishedAt,
            report.Outcome, report.Operation, report.InputHash, report.ErrorKind);
    }
}