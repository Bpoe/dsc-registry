namespace Dscregd;

public class DatabaseOptions
{
    public string Engine { get; set; } = "sqlite";
    public string ConnectionString { get; set; } = "Data Source=data/registry.db";
    public int BusyTimeoutSeconds { get; set; } = 5;
}