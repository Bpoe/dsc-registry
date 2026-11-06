namespace Dscregd;

public class RegistryOptions
{
    internal bool BootstrapOnly { get; set; }
    public DatabaseOptions Database { get; set; } = new();
    public string ReportRoot { get; set; } = "data/reports";
    public int PollMaxAgeSeconds { get; set; } = 300;
    public long MaxRequestBytes { get; set; } = 1 << 20;
    public long MaxReportBytes { get; set; } = 16 << 20;
    public TimeSpan RequestTimeout { get; set; } = TimeSpan.FromSeconds(30);
    public TimeSpan StartupTimeout { get; set; } = TimeSpan.FromSeconds(10);
    public TimeSpan ShutdownTimeout { get; set; } = TimeSpan.FromSeconds(15);
}