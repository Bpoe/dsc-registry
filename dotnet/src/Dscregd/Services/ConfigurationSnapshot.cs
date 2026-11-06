namespace Dscregd.Services;

public record ConfigurationSnapshot(long ConfigurationRevision, ConfigurationContent[] Configurations);