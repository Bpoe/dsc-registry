namespace Dscregd.Reports;

public record ConfigurationStatus(
    string ConfigurationRevision,
    string ReceivedAt,
    string StartedAt,
    string FinishedAt,
    string Outcome,
    string? Operation,
    string? InputHash,
    string? ErrorKind,
    bool HasReport,
    string ReportId)
{
    public static ConfigurationStatus FromMetadata(ReportMetadata metadata)
    {
        return new ConfigurationStatus(metadata.ConfigurationRevision, metadata.ReceivedAt,
            metadata.StartedAt, metadata.FinishedAt, metadata.Outcome, metadata.Operation,
            metadata.InputHash, metadata.ErrorKind, true, metadata.Id);
    }
}