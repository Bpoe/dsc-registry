namespace Dscregd.Services;

using System.Text.Json.Serialization;

public record ConfigurationPage(ConfigurationSummary[] Configurations,
    [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] string? NextCursor);