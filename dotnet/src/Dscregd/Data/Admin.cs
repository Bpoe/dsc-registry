namespace Dscregd.Data;

public class Admin
{
    public string Id { get; set; } = Guid.NewGuid().ToString();
    public string Name { get; set; } = "";
    public string CreatedAt { get; set; } = Timestamp.Now();
}