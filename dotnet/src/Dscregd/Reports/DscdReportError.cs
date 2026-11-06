namespace Dscregd.Reports;

using System.ComponentModel.DataAnnotations;
using System.Text.Json.Serialization;

[JsonUnmappedMemberHandling(JsonUnmappedMemberHandling.Skip)]
public class DscdReportError
{
    [JsonRequired]
    [AllowedValues("input", "start", "dsc", "output", "exit", "canceled")]
    public string Kind { get; init; } = "";

    [JsonRequired]
    public string Message { get; init; } = "";
}