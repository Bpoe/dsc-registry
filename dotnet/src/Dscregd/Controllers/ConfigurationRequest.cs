namespace Dscregd.Controllers;

using System.Text.Json;

public record ConfigurationRequest(JsonElement Template = default, JsonElement? Parameters = null);