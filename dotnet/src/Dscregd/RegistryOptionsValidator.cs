namespace Dscregd;

using Microsoft.Extensions.Options;

public class RegistryOptionsValidator : IValidateOptions<RegistryOptions>
{
    public ValidateOptionsResult Validate(string? name, RegistryOptions options)
    {
        if (options.Database.Engine is not ("sqlite" or "postgres") ||
            string.IsNullOrWhiteSpace(options.Database.ConnectionString) ||
            options.Database.BusyTimeoutSeconds is < 1 or > 60 ||
            options.StartupTimeout <= TimeSpan.Zero)
        {
            return ValidateOptionsResult.Fail("Invalid Registry database settings.");
        }

        if (!options.BootstrapOnly && (
            string.IsNullOrWhiteSpace(options.ReportRoot) ||
            options.PollMaxAgeSeconds < 1 || options.MaxRequestBytes < 1 ||
            options.MaxReportBytes < options.MaxRequestBytes ||
            options.MaxReportBytes > int.MaxValue ||
            options.RequestTimeout <= TimeSpan.Zero ||
            options.StartupTimeout <= TimeSpan.Zero || options.ShutdownTimeout <= TimeSpan.Zero))
        {
            return ValidateOptionsResult.Fail("Invalid Registry database, storage, size or timeout settings.");
        }

        return ValidateOptionsResult.Success;
    }
}