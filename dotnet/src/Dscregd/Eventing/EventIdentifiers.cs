namespace Dscregd.Eventing;

using System.Text.Json.Serialization;

public class EventIdentifiers
{
    public EventIdentifiers(string machineId, string? configurationName = null, string? reportId = null)
    {
        this.MachineId = machineId;
        this.ConfigurationName = string.IsNullOrEmpty(configurationName) ? null : configurationName;
        this.ReportId = string.IsNullOrEmpty(reportId) ? null : reportId;
    }

    [JsonPropertyName("machineId")]
    public string MachineId { get; }

    [JsonPropertyName("configurationName")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? ConfigurationName { get; }

    [JsonPropertyName("reportId")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? ReportId { get; }
}