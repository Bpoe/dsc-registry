namespace Dscregd.Reports;

using System.ComponentModel.DataAnnotations;
using System.Text;
using System.Text.Json;

public record ReportUpload(
    string Name,
    string ConfigurationRevision,
    string UploadId,
    byte[] Result,
    string StartedAt,
    string FinishedAt,
    string Outcome,
    string? Operation,
    string? InputHash,
    string? ErrorKind)
{
    public static ReportUpload FromSubmission(ReportSubmission submission, JsonSerializerOptions json)
    {
        try
        {
            Validator.ValidateObject(submission, new ValidationContext(submission), validateAllProperties: true);
            if (submission.Result.ValueKind != JsonValueKind.Object)
            {
                throw Invalid();
            }

            var report = submission.Result.Deserialize<DscdReport>(json)!;
            Validator.ValidateObject(report, new ValidationContext(report), validateAllProperties: true);
            if (report.Error is not null)
            {
                Validator.ValidateObject(report.Error, new ValidationContext(report.Error), validateAllProperties: true);
            }

            if (report.Configuration.Length == 0 ||
                report.StartedAt == DateTimeOffset.MinValue || report.FinishedAt == DateTimeOffset.MinValue ||
                report.FinishedAt < report.StartedAt ||
                report.DscResult.ValueKind is not (JsonValueKind.Undefined or JsonValueKind.Null or JsonValueKind.Object))
            {
                throw Invalid();
            }

            var validOutcome = report.Outcome switch
            {
                "succeeded" => report.Operation is not null && report.Error is null &&
                    report.ExitCode is null && report.DscResult.ValueKind == JsonValueKind.Object,
                "failed" => report.Error is not null && report.Error.Kind != "canceled",
                "canceled" => report.Error?.Kind == "canceled",
                _ => false
            };
            if (!validOutcome)
            {
                throw Invalid();
            }

            // Store the original result, never a reserialization of the summary DTO.
            return new ReportUpload(submission.ConfigurationName, submission.ConfigurationRevision.ToString(),
                submission.UploadId.ToString(), Encoding.UTF8.GetBytes(submission.Result.GetRawText()),
                submission.Result.GetProperty("startedAt").GetString()!,
                submission.Result.GetProperty("finishedAt").GetString()!, report.Outcome,
                report.Operation, report.InputHash, report.Error?.Kind);
        }
        catch (Exception error) when (error is JsonException or ValidationException or InvalidOperationException or ArgumentException)
        {
            throw Invalid();
        }
    }

    private static DomainException Invalid()
    {
        return new DomainException(400, "invalid_report", "Expected a valid wrapped dscd v1 report");
    }
}