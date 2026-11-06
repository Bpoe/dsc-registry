namespace Dscregd.Tests;

using System.Diagnostics;
using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text.Json;
using Dscregd.Data;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class GoInteropTests
{
    [TestMethod]
    public async Task BootstrapIgnoresInvalidServerAndBrokerSettings()
    {
        var root = Path.Combine(Path.GetTempPath(), "dscregd-bootstrap-options-" + Guid.NewGuid());
        Directory.CreateDirectory(root);
        try
        {
            var output = await RunAsync(FindRepository(), "dotnet",
            [
                typeof(Program).Assembly.Location, "bootstrap",
                "--Registry:Database:ConnectionString", $"Data Source={Path.Combine(root, "registry.db")}",
                "--Registry:RequestTimeout", "not-a-duration",
                "--Registry:ReportRoot", "",
                "--Eventing:Enabled", "true",
                "--Eventing:ShutdownTimeout", "not-a-duration",
                "--Eventing:Auth:Type", "plain",
                "--Eventing:Auth:PasswordEnv", "DSCREGD_MISSING_BOOTSTRAP_TEST_SECRET"
            ]);
            using var result = JsonDocument.Parse(output);
            Assert.IsTrue(Guid.TryParse(result.RootElement.GetProperty("adminId").GetString(), out _));
            Assert.IsTrue(result.RootElement.GetProperty("key").GetString()!.StartsWith("dsca_", StringComparison.Ordinal));
        }
        finally
        {
            Directory.Delete(root, recursive: true);
        }
    }

    [TestMethod]
    public async Task GoCreatedDatabaseAndUnmodifiedCliWorkWithRealDotnetListener()
    {
        if (Environment.GetEnvironmentVariable("DSCREGISTRY_TEST_GO") != "1")
        {
            Assert.Inconclusive("Set DSCREGISTRY_TEST_GO=1 to build and execute the unchanged Go CLI/server.");
        }

        var repository = FindRepository();
        var root = Path.Combine(Path.GetTempPath(), "dscregd-go-interop-" + Guid.NewGuid());
        Directory.CreateDirectory(root);
        var database = Path.Combine(root, "registry.db");
        var cli = Path.Combine(root, OperatingSystem.IsWindows() ? "dscreg.exe" : "dscreg");
        try
        {
            await RunAsync(repository, "go", ["build", "-o", cli, "./cmd/dscreg"]);
            var bootstrap = await RunAsync(repository, "go",
                ["run", "./cmd/dscregd", "bootstrap", "-database-dsn", database]);
            using var credentials = JsonDocument.Parse(bootstrap);
            var adminKey = credentials.RootElement.GetProperty("key").GetString()!;
            var adminId = credentials.RootElement.GetProperty("adminId").GetString()!;
            using var host = Program.CreateHostBuilder([])
                .ConfigureAppConfiguration((_, configuration) => configuration.AddInMemoryCollection(new Dictionary<string, string?>
                {
                    ["Registry:Database:Engine"] = "sqlite",
                    ["Registry:Database:ConnectionString"] = $"Data Source={database}",
                    ["Registry:ReportRoot"] = Path.Combine(root, "reports"),
                    ["Kestrel:Endpoints:Http:Url"] = "http://127.0.0.1:0",
                    ["Eventing:Enabled"] = "false"
                })).Build();
            await host.StartAsync();
            try
            {
                var address = host.Services.GetRequiredService<IServer>().Features.Get<IServerAddressesFeature>()!.Addresses.Single();
                var env = new Dictionary<string, string> { ["DSCREG_SERVER"] = address, ["DSCREG_API_KEY"] = adminKey };
                await RunAsync(repository, cli, ["ping"], env);
                await RunAsync(repository, cli, ["create", "machine", "dotnet-interop"], env);
                var machine = await RunAsync(repository, cli, ["get", "machine", "DOTNET-INTEROP", "-o", "json"], env);
                using var machineBody = JsonDocument.Parse(machine);
                var id = machineBody.RootElement.GetProperty("id").GetString()!;
                using var client = new HttpClient { BaseAddress = new Uri(address) };
                client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", adminKey);
                using var issued = await client.PostAsJsonAsync($"/v1/machines/{id}/api-keys", new { });
                issued.EnsureSuccessStatusCode();
                using var issuedBody = JsonDocument.Parse(await issued.Content.ReadAsStringAsync());
                using var agent = new HttpClient { BaseAddress = new Uri(address) };
                agent.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", issuedBody.RootElement.GetProperty("key").GetString());
                var template = Path.Combine(root, "template.json");
                await File.WriteAllTextAsync(template, "{\"resources\":[],\"opaque\":{\"n\":9007199254740993123456789}}");
                await RunAsync(repository, cli, ["apply", "configuration", "example", "--machine", id, "-f", template], env);
                await RunAsync(repository, cli, ["apply", "configuration", "example", "--machine", id, "-f", template], env);
                await RunAsync(repository, cli, ["get", "configurations", "--machine", id, "-o", "json"], env);
                using var desired = await agent.GetAsync("/v1/agent/configurations");
                desired.EnsureSuccessStatusCode();
                using var desiredBody = JsonDocument.Parse(await desired.Content.ReadAsStringAsync());
                var revision = desiredBody.RootElement.GetProperty("configurations")[0].GetProperty("revision").GetString()!;
                using var uploaded = await agent.PostAsJsonAsync("/v1/agent/reports", new
                {
                    configurationName = "example",
                    configurationRevision = revision,
                    uploadId = Guid.NewGuid().ToString(),
                    result = new
                    {
                        schemaVersion = 1,
                        configuration = "dscregsync/example.json",
                        operation = "test",
                        startedAt = "2026-11-06T00:00:00Z",
                        finishedAt = "2026-11-06T00:00:01Z",
                        durationMs = 1000,
                        outcome = "succeeded",
                        dscResult = new { hadErrors = false }
                    }
                });
                Assert.AreEqual(HttpStatusCode.Created, uploaded.StatusCode);
                await RunAsync(repository, cli, ["get", "status", "--machine", id, "-o", "json"], env);
                await RunAsync(repository, cli, ["get", "reports", "--machine", id, "-o", "json"], env);
                using var scope = host.Services.CreateScope();
                var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
                Assert.AreEqual(adminId, (await db.Admins.FindAsync(adminId))!.Id);
            }
            finally
            {
                await host.StopAsync();
            }
        }
        finally
        {
            Microsoft.Data.Sqlite.SqliteConnection.ClearAllPools();
            Directory.Delete(root, recursive: true);
        }
    }

    private static string FindRepository()
    {
        var directory = new DirectoryInfo(AppContext.BaseDirectory);
        while (directory is not null)
        {
            if (File.Exists(Path.Combine(directory.FullName, "go.mod")))
            {
                return directory.FullName;
            }

            directory = directory.Parent;
        }

        throw new InvalidOperationException("Repository root not found.");
    }

    private static async Task<string> RunAsync(string cwd, string executable, string[] arguments,
        Dictionary<string, string>? environment = null)
    {
        var start = new ProcessStartInfo(executable)
        {
            WorkingDirectory = cwd,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false
        };
        foreach (var argument in arguments)
        {
            start.ArgumentList.Add(argument);
        }

        if (environment is not null)
        {
            foreach (var (key, value) in environment)
            {
                start.Environment[key] = value;
            }
        }

        using var process = Process.Start(start)!;
        var output = process.StandardOutput.ReadToEndAsync();
        var errors = process.StandardError.ReadToEndAsync();
        using var timeout = new CancellationTokenSource(TimeSpan.FromMinutes(2));
        try
        {
            await process.WaitForExitAsync(timeout.Token);
            await errors;
            if (process.ExitCode != 0)
            {
                // Do not expose bootstrap credentials or client diagnostic values in test output.
                throw new InvalidOperationException($"External command {Path.GetFileName(executable)} failed with exit code {process.ExitCode}.");
            }

            return await output;
        }
        finally
        {
            if (!process.HasExited)
            {
                process.Kill(entireProcessTree: true);
                await process.WaitForExitAsync();
            }
        }
    }
}