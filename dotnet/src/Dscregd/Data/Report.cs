namespace Dscregd.Data;

public class Report
{
    public string Id { get; set; } = Guid.NewGuid().ToString();
    public string MachineId { get; set; } = "";
    public long Sequence { get; set; }
    public string Name { get; set; } = "";
    public string ConfigurationRevision { get; set; } = "";
    public string ReceivedAt { get; set; } = Timestamp.Now();
    public string StartedAt { get; set; } = "";
    public string FinishedAt { get; set; } = "";
    public string Outcome { get; set; } = "";
    public string? Operation { get; set; }
    public string? InputHash { get; set; }
    public string? ErrorKind { get; set; }
    public string ReportPath { get; set; } = "";
    public string UploadId { get; set; } = "";
    public byte[]? ResultSha256 { get; set; }
}