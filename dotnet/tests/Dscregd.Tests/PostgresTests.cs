namespace Dscregd.Tests;

using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text.Json;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class PostgresTests
{
    [TestMethod]
    public async Task SameProviderScenarioPreservesAuthRevisionsReportsAndNameUniqueness()
    {
        if (string.IsNullOrEmpty(Environment.GetEnvironmentVariable("DSCREGISTRY_TEST_POSTGRES_DOTNET")))
        {
            Assert.Inconclusive("Set DSCREGISTRY_TEST_POSTGRES_DOTNET to a disposable PostgreSQL database connection string.");
        }

        await using var factory = new RegistryFactory("postgres");
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("Straße");
        using var conflict = await factory.Client.PostAsJsonAsync("/v1/machines", new { name = "STRASSE" });
        Assert.AreEqual(HttpStatusCode.Conflict, conflict.StatusCode);
        var machineKey = await factory.IssueMachineKeyAsync(id);
        var route = $"/v1/machines/{id}/configurations/example";
        using var first = await factory.Client.PutAsJsonAsync(route, new { template = new { n = 1 } });
        first.EnsureSuccessStatusCode();
        using var firstBody = JsonDocument.Parse(await first.Content.ReadAsStringAsync());
        var revision = firstBody.RootElement.GetProperty("revision").GetString();
        using var noOp = await factory.Client.PutAsJsonAsync(route, new { template = new { n = 1.0 } });
        noOp.EnsureSuccessStatusCode();
        using var noOpBody = JsonDocument.Parse(await noOp.Content.ReadAsStringAsync());
        Assert.AreEqual(revision, noOpBody.RootElement.GetProperty("revision").GetString());
        using var agent = factory.CreateClient();
        agent.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", machineKey);
        using var poll = await agent.GetAsync("/v1/agent/configurations");
        poll.EnsureSuccessStatusCode();
        var etag = poll.Headers.ETag!.ToString();
        using var uploaded = await agent.PostAsJsonAsync("/v1/agent/reports", new
        {
            configurationName = "example",
            configurationRevision = revision,
            uploadId = Guid.NewGuid().ToString(),
            result = new
            {
                schemaVersion = 1,
                configuration = "custom/example.yaml",
                operation = "export",
                startedAt = "2026-11-06T00:00:00Z",
                finishedAt = "2026-11-06T00:00:01Z",
                durationMs = 1000,
                outcome = "succeeded",
                dscResult = new { resources = Array.Empty<object>() }
            }
        });
        Assert.AreEqual(HttpStatusCode.Created, uploaded.StatusCode);
        using var unchanged = await agent.GetAsync("/v1/agent/configurations");
        Assert.AreEqual(etag, unchanged.Headers.ETag!.ToString());
        using var status = await factory.Client.GetAsync($"/v1/machines/{id}/status");
        status.EnsureSuccessStatusCode();
        using var statusBody = JsonDocument.Parse(await status.Content.ReadAsStringAsync());
        Assert.AreEqual("succeeded", statusBody.RootElement.GetProperty("outcome").GetString());
        using var forbidden = await agent.GetAsync($"/v1/machines/{id}");
        Assert.AreEqual(HttpStatusCode.Forbidden, forbidden.StatusCode);
    }
}