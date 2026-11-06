namespace Dscregd.Data;

public class RegistrationToken
{
    public byte[] TokenHash { get; set; } = [];
    public string ExpiresAt { get; set; } = "";
}