namespace Dscregd;

using System.Text.Json;
using System.Text.Json.Serialization;
using Dscregd.Authentication;
using Dscregd.Data;
using Dscregd.Eventing;
using Dscregd.Http;
using Dscregd.Reports;
using Dscregd.Services;
using Microsoft.AspNetCore.Diagnostics;
using Microsoft.AspNetCore.Diagnostics.HealthChecks;
using Microsoft.AspNetCore.Http.Timeouts;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Mvc.Controllers;
using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;

public class Startup
{
    private readonly IConfiguration configuration;

    public Startup(IConfiguration configuration)
    {
        this.configuration = configuration;
    }

    public void ConfigureServices(IServiceCollection services)
    {
        services.AddOptions<RegistryOptions>().Bind(this.configuration.GetSection("Registry")).ValidateOnStart();
        services.AddSingleton<IValidateOptions<RegistryOptions>, RegistryOptionsValidator>();
        AddDatabase(services);
        services.AddRegistryAuthentication();
        services.AddSingleton<ReportStorage>();
        services.AddScoped<ConfigurationService>();
        services.AddScoped<ReportService>();
        services.AddScoped<StatusService>();
        services.AddControllers(options =>
        {
            options.Filters.Add<RequestBodyLimitFilter>();
        }).AddJsonOptions(options =>
        {
            options.JsonSerializerOptions.PropertyNameCaseInsensitive = false;
            options.JsonSerializerOptions.NumberHandling = JsonNumberHandling.Strict;
            options.JsonSerializerOptions.AllowDuplicateProperties = false;
            options.JsonSerializerOptions.RespectNullableAnnotations = true;
            options.JsonSerializerOptions.UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow;
            options.JsonSerializerOptions.MaxDepth = 128;
        });
        services.AddSingleton(provider => provider.GetRequiredService<IOptions<JsonOptions>>().Value.JsonSerializerOptions);
        services.Configure<ApiBehaviorOptions>(options =>
        {
            options.SuppressMapClientErrors = true;
            options.InvalidModelStateResponseFactory = context =>
            {
                var controller = ((ControllerActionDescriptor)context.ActionDescriptor).ControllerName;
                var fields = context.ModelState.Where(entry => entry.Value?.Errors.Count > 0 && !entry.Key.StartsWith('$'))
                    .Select(entry => entry.Key.Split('.').Last()).ToHashSet(StringComparer.OrdinalIgnoreCase);
                var code = controller switch
                {
                    "Configurations" => "invalid_configuration",
                    "Reports" => "invalid_report",
                    _ => "invalid_json"
                };
                if (fields.Contains("id") || fields.Contains("reportId") || fields.Contains("keyId"))
                {
                    code = "invalid_id";
                }
                else if (fields.Contains("name"))
                {
                    code = "invalid_name";
                }
                else if (fields.Contains("limit") || fields.Contains("cursor"))
                {
                    code = "invalid_pagination";
                }
                else if (fields.Contains("ttlSeconds"))
                {
                    code = "invalid_ttl";
                }

                return new BadRequestObjectResult(new { error = new { code, message = "Invalid request" } });
            };
        });
        services.AddRequestTimeouts();
        services.AddOptions<RequestTimeoutOptions>().Configure<IOptions<RegistryOptions>>((options, registry) =>
        {
            options.DefaultPolicy = new RequestTimeoutPolicy
            {
                Timeout = registry.Value.RequestTimeout,
                TimeoutStatusCode = StatusCodes.Status503ServiceUnavailable,
                WriteTimeoutResponse = context => context.Response.WriteAsJsonAsync(new
                {
                    error = new { code = "database_unavailable", message = "Registry operation timed out" }
                })
            };
        });
        services.AddHealthChecks();
        services.AddHostedService<InitializationService>();
        services.AddOptions<Microsoft.AspNetCore.Server.Kestrel.Core.KestrelServerOptions>()
            .Configure<IOptions<RegistryOptions>>((options, registry) =>
            {
                options.Limits.MaxRequestBodySize = registry.Value.MaxReportBytes;
                options.Limits.MaxRequestHeadersTotalSize = 64 << 10;
                options.Limits.RequestHeadersTimeout = TimeSpan.FromSeconds(5);
                options.Limits.KeepAliveTimeout = TimeSpan.FromMinutes(1);
            });
    }

    public void Configure(IApplicationBuilder app, IWebHostEnvironment env)
    {
        app.Use(async (context, next) =>
        {
            // ASP.NET exception handling adds no-cache at response start. Restore
            // the existing JSON error contract after its callback has run.
            context.Response.OnStarting(() =>
            {
                if (context.Response.StatusCode >= 400 &&
                    context.Response.ContentType?.StartsWith("application/json", StringComparison.Ordinal) == true)
                {
                    context.Response.Headers.CacheControl = "no-store";
                    context.Response.Headers.Remove("Pragma");
                    context.Response.Headers.Remove("Expires");
                }

                return Task.CompletedTask;
            });
            await next();
        });
        app.UseExceptionHandler(error => error.Run(async context =>
        {
            var feature = context.Features.Get<IExceptionHandlerFeature>();
            var exception = feature?.Error;
            var logger = context.RequestServices.GetRequiredService<ILogger<Startup>>();
            var status = 500;
            var code = "internal_error";
            var message = "Internal server error";
            if (exception is DomainException domain)
            {
                status = domain.Status;
                code = domain.Code;
                message = domain.Message;
            }
            else if (exception is JsonException)
            {
                status = 400;
                var controller = feature?.Endpoint?.Metadata.GetMetadata<ControllerActionDescriptor>()?.ControllerName;
                code = controller == "Configurations" ? "invalid_configuration" : "invalid_json";
                message = "Expected a valid JSON request";
            }
            else if (exception is BadHttpRequestException requestError)
            {
                status = requestError.StatusCode;
                code = status == 413 ? "body_too_large" : "invalid_request";
                message = status == 413 ? "Request body too large" : "Invalid HTTP request";
            }
            else if (exception is System.Data.Common.DbException or DbUpdateException or OperationCanceledException)
            {
                status = 503;
                code = "database_unavailable";
                message = "Database unavailable";
                logger.LogError("Registry operation failed; reason={Reason}", "database_or_deadline_failure");
            }
            else
            {
                logger.LogError("Registry operation failed; reason={Reason}", "unexpected_failure");
            }

            context.Response.StatusCode = status;
            context.Response.ContentType = "application/json";
            context.Response.Headers.CacheControl = "no-store";
            context.Response.Headers.Remove("ETag");
            context.Response.Headers.Remove("Vary");
            if (!HttpMethods.IsHead(context.Request.Method))
            {
                await context.Response.WriteAsJsonAsync(new { error = new { code, message } });
            }
        }));
        app.UseRouting();
        app.UseRequestTimeouts();
        app.Use(async (context, next) =>
        {
            context.Response.Headers.CacheControl = "no-store";
            await next();
        });
        app.UseAuthentication();
        app.UseAuthorization();
        app.UseEndpoints(endpoints =>
        {
            endpoints.MapControllers();
            endpoints.MapHealthChecks("/health/live", Health("live")).WithMetadata(new HttpMethodMetadata(["GET", "HEAD"]));
            endpoints.MapHealthChecks("/health/ready", Health("ready")).WithMetadata(new HttpMethodMetadata(["GET", "HEAD"]));
        });
    }

    internal static void AddDatabase(IServiceCollection services)
    {
        services.AddDbContext<RegistryDbContext>((provider, options) =>
        {
            var database = provider.GetRequiredService<IOptions<RegistryOptions>>().Value.Database;
            if (database.Engine == "sqlite")
            {
                var connection = new SqliteConnectionStringBuilder(database.ConnectionString)
                {
                    ForeignKeys = true,
                    DefaultTimeout = database.BusyTimeoutSeconds
                };
                options.UseSqlite(connection.ConnectionString);
            }
            else
            {
                options.UseNpgsql(database.ConnectionString);
            }

            // SQL literals and provider exceptions can contain protected templates or DSNs.
            options.EnableSensitiveDataLogging(false);
            options.EnableDetailedErrors(false);
            options.ConfigureWarnings(warnings => warnings.Ignore(
                Microsoft.EntityFrameworkCore.Diagnostics.RelationalEventId.PendingModelChangesWarning));
        });
    }

    private static HealthCheckOptions Health(string status)
    {
        return new HealthCheckOptions
        {
            Predicate = _ => false,
            ResponseWriter = async (context, _) =>
            {
                context.Response.ContentType = "application/json";
                if (!HttpMethods.IsHead(context.Request.Method))
                {
                    await context.Response.WriteAsync($"{{\"status\":\"{status}\"}}");
                }
            }
        };
    }
}