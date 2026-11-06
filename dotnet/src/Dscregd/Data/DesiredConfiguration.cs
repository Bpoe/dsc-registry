namespace Dscregd.Data;

public class DesiredConfiguration
{
    public const string NamePattern = @"^(?!.*(?:\.parameters|\.)$)[a-z0-9][a-z0-9._-]{0,63}$";

    public string MachineId { get; set; } = "";
    public string Name { get; set; } = "";
    public string Revision { get; set; } = Guid.NewGuid().ToString();
    public string TemplateJson { get; set; } = "{}";
    public string? ParametersJson { get; set; }
    public byte[] ContentSha256 { get; set; } = [];
    public string UpdatedAt { get; set; } = Timestamp.Now();
}