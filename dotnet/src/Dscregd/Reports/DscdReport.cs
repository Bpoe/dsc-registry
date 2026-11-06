namespace Dscregd.Reports;

using System.ComponentModel.DataAnnotations;
using System.Text.Json;
using System.Text.Json.Serialization;

[JsonUnmappedMemberHandling(JsonUnmappedMemberHandling.Skip)]
public class DscdReport
{
    [JsonRequired]
    [AllowedValues(1)]
    public int SchemaVersion { get; init; }

    [JsonRequired]
    public string Configuration { get; init; } = "";

    [AllowedValues(null, "set", "test", "export")]
    public string? Operation { get; init; }

    public string? Parameters { get; init; }

    [RegularExpression("^sha256:[0-9a-f]{64}$")]
    public string? InputHash { get; init; }

    [JsonRequired]
    public DateTimeOffset StartedAt { get; init; }

    [JsonRequired]
    public DateTimeOffset FinishedAt { get; init; }

    [JsonRequired]
    [Range(typeof(long), "0", "9223372036854775807")]
    public long DurationMs { get; init; }

    [JsonRequired]
    [AllowedValues("succeeded", "failed", "canceled")]
    public string Outcome { get; init; } = "";

    [Range(typeof(long), "0", "9223372036854775807")]
    public long? ExitCode { get; init; }

    public JsonElement DscResult { get; init; }

    public DscdReportError? Error { get; init; }

    public string? Stderr { get; init; }
}