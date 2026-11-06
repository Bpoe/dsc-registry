namespace Dscregd;

using Dscregd.Eventing;
using Microsoft.Extensions.Logging.Console;
using Microsoft.Extensions.Options;

public class Program
{
    public static async Task<int> Main(string[] args)
    {
        try
        {
            if (args.FirstOrDefault() == "bootstrap")
            {
                return await BootstrapCommand.RunAsync(args[1..]);
            }

            using var host = CreateHostBuilder(args).Build();
            await host.RunAsync();
            return 0;
        }
        catch
        {
            // Provider/transport exceptions may contain credentials, bodies or private paths.
            await Console.Error.WriteLineAsync("dscregd startup or shutdown failed; check protected configuration and dependencies.");
            return 1;
        }
    }

    public static IHostBuilder CreateHostBuilder(string[] args)
    {
        return Host.CreateDefaultBuilder(args)
            .ConfigureLogging(logging =>
            {
                logging.ClearProviders();
                logging.AddJsonConsole();
                logging.Services.Configure<ConsoleLoggerOptions>(options => options.LogToStandardErrorThreshold = LogLevel.Trace);
                logging.AddFilter("Microsoft.EntityFrameworkCore", LogLevel.None);
                logging.AddFilter("Microsoft.AspNetCore", LogLevel.None);
                logging.AddFilter("Microsoft.Extensions.Hosting.Internal.Host", LogLevel.None);
            })
            .ConfigureServices((context, services) =>
            {
                services.AddRegistryEventing(context.Configuration);
                services.AddOptions<HostOptions>().Configure<IOptions<RegistryOptions>, IOptions<Eventing.EventingOptions>>((options, registry, events) =>
                {
                    options.ShutdownTimeout = registry.Value.ShutdownTimeout + events.Value.ShutdownTimeout + TimeSpan.FromSeconds(1);
                    options.ServicesStartConcurrently = false;
                    options.ServicesStopConcurrently = false;
                });
            })
            .ConfigureWebHostDefaults(web => web.UseStartup<Startup>().UseUrls("http://127.0.0.1:8080"));
    }
}