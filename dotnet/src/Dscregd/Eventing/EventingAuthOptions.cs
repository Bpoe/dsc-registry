namespace Dscregd.Eventing;

public class EventingAuthOptions
{
    public string Type { get; set; } = "anonymous";

    public string Username { get; set; } = string.Empty;

    public string PasswordEnv { get; set; } = string.Empty;
}