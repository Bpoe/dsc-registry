namespace Dscregd.Tests;

using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text.Json;
using Dscregd.Services;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Mvc.Testing;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;

public class RegistryFactory : WebApplicationFactory<Program>
{
    private readonly string engine;
    private readonly string postgresSchema = "dotnet_test_" + Guid.NewGuid().ToString("N");
    private string? postgresConnection;

    public RegistryFactory(string engine = "sqlite")
    {
        this.engine = engine;
        this.UseKestrel(0);
        this.ClientOptions.AllowAutoRedirect = false;
    }

    public string Root { get; } = Path.Combine(Path.GetTempPath(), "dscregd-dotnet-" + Guid.NewGuid());

    public string DatabasePath => Path.Combine(this.Root, "registry.db");

    public string AdminKey { get; private set; } = "";

    public string AdminId { get; private set; } = "";

    public HttpClient Client { get; private set; } = null!;

    public async Task InitializeAsync()
    {
        Directory.CreateDirectory(this.Root);
        if (this.engine == "postgres")
        {
            var connection = Environment.GetEnvironmentVariable("DSCREGISTRY_TEST_POSTGRES_DOTNET")
                ?? throw new InvalidOperationException("PostgreSQL test connection not configured.");
            var settings = new Npgsql.NpgsqlConnectionStringBuilder(connection) { SearchPath = this.postgresSchema };
            this.postgresConnection = settings.ConnectionString;
            await using var database = new Npgsql.NpgsqlConnection(connection);
            await database.OpenAsync();
            await using var command = database.CreateCommand();
            command.CommandText = $"CREATE SCHEMA \"{this.postgresSchema}\"";
            await command.ExecuteNonQueryAsync();
        }

        this.Client = this.CreateClient();
        using var scope = this.Services.CreateScope();
        var result = await scope.ServiceProvider.GetRequiredService<CredentialService>()
            .BootstrapAsync("administrator", CancellationToken.None);
        this.AdminKey = result.ApiKey;
        this.AdminId = result.AdminId;
        this.Client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", this.AdminKey);
    }

    public async Task<string> CreateMachineAsync(string name)
    {
        using var response = await this.Client.PostAsJsonAsync("/v1/machines", new { name });
        response.EnsureSuccessStatusCode();
        using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        return body.RootElement.GetProperty("id").GetString()!;
    }

    public async Task<string> IssueMachineKeyAsync(string id)
    {
        using var response = await this.Client.PostAsJsonAsync($"/v1/machines/{id}/api-keys", new { });
        response.EnsureSuccessStatusCode();
        using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        return body.RootElement.GetProperty("key").GetString()!;
    }

    public override async ValueTask DisposeAsync()
    {
        this.Client?.Dispose();
        await base.DisposeAsync();
        if (this.postgresConnection is not null)
        {
            await using var database = new Npgsql.NpgsqlConnection(this.postgresConnection);
            await database.OpenAsync();
            await using var command = database.CreateCommand();
            command.CommandText = $"DROP SCHEMA \"{this.postgresSchema}\" CASCADE";
            await command.ExecuteNonQueryAsync();
            Npgsql.NpgsqlConnection.ClearPool(database);
        }

        Microsoft.Data.Sqlite.SqliteConnection.ClearAllPools();
        if (Directory.Exists(this.Root))
        {
            Directory.Delete(this.Root, recursive: true);
        }
    }

    protected override void ConfigureWebHost(IWebHostBuilder builder)
    {
        builder.ConfigureAppConfiguration((_, configuration) => configuration.AddInMemoryCollection(
            new Dictionary<string, string?>
            {
                ["Registry:Database:Engine"] = this.engine,
                ["Registry:Database:ConnectionString"] = this.postgresConnection ?? $"Data Source={this.DatabasePath}",
                ["Registry:ReportRoot"] = Path.Combine(this.Root, "reports"),
                ["Registry:PollMaxAgeSeconds"] = "37",
                ["Eventing:Enabled"] = "false"
            }));
    }
}