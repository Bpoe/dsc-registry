namespace Dscregd.Eventing;

using System;
using System.Text.Json.Serialization;

public class RegistryEvent
{
    public RegistryEvent(string type, string machineId, string? configurationName = null, string? reportId = null)
    {
        this.Id = Guid.NewGuid();
        this.Type = type;
        this.Time = DateTimeOffset.UtcNow;
        this.Data = new EventIdentifiers(machineId, configurationName, reportId);
        this.Subject = "machines/" + machineId;
        if (!string.IsNullOrEmpty(configurationName))
        {
            this.Subject += "/configurations/" + configurationName;
        }

        if (!string.IsNullOrEmpty(reportId))
        {
            this.Subject = "machines/" + machineId + "/reports/" + reportId;
        }
    }

    [JsonPropertyName("specversion")]
    public string SpecVersion
    {
        get
        {
            return "1.0";
        }
    }

    [JsonPropertyName("id")]
    public Guid Id { get; }

    [JsonPropertyName("source")]
    public string Source
    {
        get
        {
            return "dsc-registry";
        }
    }

    [JsonPropertyName("type")]
    public string Type { get; }

    [JsonPropertyName("subject")]
    public string Subject { get; }

    [JsonPropertyName("time")]
    public DateTimeOffset Time { get; }

    [JsonPropertyName("datacontenttype")]
    public string DataContentType
    {
        get
        {
            return "application/json";
        }
    }

    [JsonPropertyName("data")]
    public EventIdentifiers Data { get; }
}