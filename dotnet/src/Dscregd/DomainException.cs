namespace Dscregd;

public class DomainException(int status, string code, string message) : Exception(message)
{
    public int Status { get; } = status;
    public string Code { get; } = code;
    public static DomainException MachineNotFound() => new(404, "machine_not_found", "Machine not found");
    public static DomainException ConfigurationNotFound() => new(404, "configuration_not_found", "Configuration not found");
    public static DomainException ReportNotFound() => new(404, "report_not_found", "Report not found");
}