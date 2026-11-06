namespace Dscregd.Services;

using System.Text.Json.Serialization;

public record BootstrapResult(
    string AdminId,
    string KeyId,
    [property: JsonPropertyName("key")] string ApiKey,
    string CreatedAt);