namespace Dscregd.Services;

using System.Collections.Generic;
using System.Text.Json.Serialization;

public record KeyPage(
    IReadOnlyList<KeyMetadata> Keys,
    [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] string? NextCursor);