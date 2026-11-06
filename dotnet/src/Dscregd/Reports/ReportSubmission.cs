namespace Dscregd.Reports;

using System.ComponentModel.DataAnnotations;
using System.Text.Json;
using System.Text.Json.Serialization;
using Dscregd.Data;

public class ReportSubmission
{
    [JsonRequired]
    [Required]
    [RegularExpression(DesiredConfiguration.NamePattern)]
    public string ConfigurationName { get; init; } = "";

    [JsonRequired]
    public Guid ConfigurationRevision { get; init; }

    [JsonRequired]
    public Guid UploadId { get; init; }

    [JsonRequired]
    public JsonElement Result { get; init; }
}