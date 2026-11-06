namespace Dscregd.Controllers;

using System.ComponentModel.DataAnnotations;

public class RegistrationTokenRequest
{
    [Range(1, 86400)]
    public int TtlSeconds { get; set; } = 3600;
}