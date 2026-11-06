namespace Dscregd.Data;

public class Machine
{
    public string Id { get; set; } = Guid.NewGuid().ToString();
    public string Name { get; set; } = "";
    public string NameFolded { get; set; } = "";
    public long ConfigurationRevision { get; set; }
    public string CreatedAt { get; set; } = Timestamp.Now();
}