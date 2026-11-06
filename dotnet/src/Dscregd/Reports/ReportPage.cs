namespace Dscregd.Reports;

using System.Text.Json.Serialization;

public record ReportPage(
    ReportMetadata[] Reports,
    [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] string? NextCursor);