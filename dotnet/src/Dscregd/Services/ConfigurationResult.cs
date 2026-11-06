namespace Dscregd.Services;

using System.Text.Json;
using System.Text.Json.Serialization;

public record ConfigurationResult(string Name, string Revision, long MachineConfigurationRevision,
    JsonElement Template, [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] JsonElement? Parameters);