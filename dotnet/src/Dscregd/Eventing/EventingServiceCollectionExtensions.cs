namespace Dscregd.Eventing;

using System;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.DependencyInjection.Extensions;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;

public static class EventingServiceCollectionExtensions
{
    public static IServiceCollection AddRegistryEventing(this IServiceCollection services, IConfiguration configuration)
    {
        ArgumentNullException.ThrowIfNull(services);
        ArgumentNullException.ThrowIfNull(configuration);
        var options = EventingOptions.Read(configuration);
        services.AddSingleton<IOptions<EventingOptions>>(Options.Create(options));
        if (!options.Enabled)
        {
            services.AddSingleton(new EventDispatcher());
            return services;
        }

        // Hosted-service construction loads local TLS material and secrets,
        // before any listener starts; neither construction nor StartAsync dials.
        services.TryAddSingleton<IEventPublisher, AmqpEventPublisher>();
        services.AddSingleton<EventWorker>();
        services.AddSingleton<EventDispatcher>();
        services.AddSingleton<IHostedService>(provider => provider.GetRequiredService<EventWorker>());
        return services;
    }
}