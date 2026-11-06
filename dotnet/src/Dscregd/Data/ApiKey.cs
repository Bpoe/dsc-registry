namespace Dscregd.Data;

public class ApiKey
{
    public string KeyId { get; set; } = "";
    public string OwnerType { get; set; } = "";
    public string OwnerId { get; set; } = "";
    public byte[] SecretHash { get; set; } = [];
    public string CreatedAt { get; set; } = Timestamp.Now();
    public string? RevokedAt { get; set; }
    public string? Description { get; set; }
}