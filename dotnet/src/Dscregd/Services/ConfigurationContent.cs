namespace Dscregd.Services;

using System.Text.Json;
using System.Text.Json.Serialization;

public record ConfigurationContent(string Name, string Revision, JsonElement Template,
    [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] JsonElement? Parameters);