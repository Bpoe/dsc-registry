namespace Dscregd;

using Dscregd.Data;
using Dscregd.Reports;
using Microsoft.Extensions.Options;

public class InitializationService : IHostedLifecycleService
{
    private readonly IServiceProvider services;
    private readonly RegistryOptions options;
    private readonly ReportStorage storage;
    private readonly ILogger<InitializationService> logger;

    public InitializationService(IServiceProvider services, IOptions<RegistryOptions> options,
        ReportStorage storage, ILogger<InitializationService> logger)
    {
        this.services = services;
        this.options = options.Value;
        this.storage = storage;
        this.logger = logger;
    }

    public async Task StartingAsync(CancellationToken cancellationToken)
    {
        using var startup = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        startup.CancelAfter(this.options.StartupTimeout);
        await DatabaseInitialization.InitializeAsync(this.services, startup.Token);
        await this.storage.InitializeAsync(startup.Token);
        this.logger.LogInformation("Registry dependencies initialized");
    }

    public Task StartAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StartedAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StoppingAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StopAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StoppedAsync(CancellationToken cancellationToken) => Task.CompletedTask;
}