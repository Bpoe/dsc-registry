namespace Dscregd;

using System.Text.Json;
using Dscregd.Authentication;
using Dscregd.Data;
using Dscregd.Services;
using Microsoft.Extensions.Options;

public static class BootstrapCommand
{
    public static async Task<int> RunAsync(string[] args)
    {
        using var host = Host.CreateDefaultBuilder(args)
            .ConfigureLogging(logging => logging.ClearProviders())
            .ConfigureServices((context, services) =>
            {
                services.AddOptions<RegistryOptions>().Configure(options =>
                {
                    options.BootstrapOnly = true;
                    context.Configuration.GetSection("Registry:Database").Bind(options.Database);
                    options.StartupTimeout = context.Configuration.GetValue<TimeSpan?>("Registry:StartupTimeout")
                        ?? TimeSpan.FromSeconds(10);
                });
                services.AddSingleton<IValidateOptions<RegistryOptions>, RegistryOptionsValidator>();
                Startup.AddDatabase(services);
                services.AddRegistryAuthentication();
            })
            .Build();
        var options = host.Services.GetRequiredService<IOptions<RegistryOptions>>().Value;
        using var timeout = new CancellationTokenSource(options.StartupTimeout);
        await DatabaseInitialization.InitializeAsync(host.Services, timeout.Token);
        using var scope = host.Services.CreateScope();
        var credentials = scope.ServiceProvider.GetRequiredService<CredentialService>();
        var name = host.Services.GetRequiredService<IConfiguration>()["name"] ?? "administrator";
        var result = await credentials.BootstrapAsync(name, timeout.Token);
        try
        {
            await Console.Out.WriteLineAsync(JsonSerializer.Serialize(result, JsonSerializerOptions.Web));
            await Console.Out.FlushAsync();
        }
        catch (IOException)
        {
            using var recovery = new CancellationTokenSource(options.StartupTimeout);
            await credentials.RevokeAsync("admin", result.AdminId, result.KeyId, recovery.Token);
            await Console.Error.WriteLineAsync("Bootstrap output failed; new key revoked. Retry with protected output.");
            return 1;
        }

        return 0;
    }
}