namespace Dscregd.Services;

using System.Collections.Generic;
using System.Text.Json.Serialization;

public record MachinePage(
    IReadOnlyList<MachineDetails> Machines,
    [property: JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] string? NextCursor);