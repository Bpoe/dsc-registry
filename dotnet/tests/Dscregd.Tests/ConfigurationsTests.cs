namespace Dscregd.Tests;

using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using Dscregd.Controllers;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class ConfigurationsTests
{
    [TestMethod]
    public async Task CanonicalPutsPreserveRevisionsAndParameterPresence()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("canonical-host");
        var route = $"/v1/machines/{id}/configurations/example";
        var first = await PutAsync(factory.Client, route,
            "{\"template\":{\"z\":9007199254740993123456789,\"n\":1.20,\"s\":\"<>&\\uD83D\\uDE00\",\"x\":1e999999},\"parameters\":null}");
        var second = await PutAsync(factory.Client, route,
            "{\"template\":{\"x\":10e999998,\"s\":\"\\u003c\\u003e\\u0026😀\",\"n\":12e-1,\"z\":9007199254740993123456789.000}}");
        Assert.AreEqual(first.GetProperty("revision").GetString(), second.GetProperty("revision").GetString());
        Assert.AreEqual(1L, second.GetProperty("machineConfigurationRevision").GetInt64());
        Assert.IsFalse(second.TryGetProperty("parameters", out _));
        var third = await PutAsync(factory.Client, route, "{\"template\":{},\"parameters\":{}}");
        Assert.AreNotEqual(first.GetProperty("revision").GetString(), third.GetProperty("revision").GetString());
        Assert.AreEqual(JsonValueKind.Object, third.GetProperty("parameters").ValueKind);
        Assert.AreEqual(2L, third.GetProperty("machineConfigurationRevision").GetInt64());
        Assert.AreEqual("9007199254740993123456789", first.GetProperty("template").GetProperty("z").GetRawText());
    }

    [TestMethod]
    public async Task PollingUsesMachineScopedValidatorsAndOnlyDesiredMutationsChangeThem()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("poll-host");
        var other = await factory.CreateMachineAsync("other-host");
        var key = await factory.IssueMachineKeyAsync(id);
        var otherKey = await factory.IssueMachineKeyAsync(other);
        using var agent = factory.CreateClient();
        agent.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", key);
        using var first = await agent.GetAsync("/v1/agent/configurations");
        Assert.AreEqual(HttpStatusCode.OK, first.StatusCode);
        var tag = first.Headers.ETag!.ToString();
        StringAssert.Contains(tag, id);
        Assert.IsTrue(first.Headers.CacheControl!.Private);
        Assert.AreEqual(TimeSpan.FromSeconds(37), first.Headers.CacheControl.MaxAge);
        CollectionAssert.Contains(first.Headers.Vary.ToArray(), "Authorization");
        using var conditional = new HttpRequestMessage(HttpMethod.Head, "/v1/agent/configurations");
        conditional.Headers.TryAddWithoutValidation("If-None-Match", "W/" + tag);
        using var cached = await agent.SendAsync(conditional);
        Assert.AreEqual(HttpStatusCode.NotModified, cached.StatusCode);
        Assert.AreEqual(0, (await cached.Content.ReadAsByteArrayAsync()).Length);
        Assert.AreEqual(tag, cached.Headers.ETag!.ToString());
        await factory.IssueMachineKeyAsync(id);
        using var stillSame = await agent.GetAsync("/v1/agent/configurations");
        Assert.AreEqual(tag, stillSame.Headers.ETag!.ToString());
        await PutAsync(factory.Client, $"/v1/machines/{id}/configurations/one", "{\"template\":{}}");
        using var changed = await agent.GetAsync("/v1/agent/configurations");
        Assert.AreNotEqual(tag, changed.Headers.ETag!.ToString());
        agent.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("ApiKey", otherKey);
        using var isolated = await agent.GetAsync("/v1/agent/configurations");
        using var isolatedBody = JsonDocument.Parse(await isolated.Content.ReadAsStringAsync());
        Assert.AreEqual(0, isolatedBody.RootElement.GetProperty("configurations").GetArrayLength());
        Assert.AreNotEqual(tag, isolated.Headers.ETag!.ToString());
        using var ignored = await agent.GetAsync("/v1/agent/configurations?machineId=" + id);
        Assert.AreEqual(HttpStatusCode.OK, ignored.StatusCode);
        using var ignoredBody = JsonDocument.Parse(await ignored.Content.ReadAsStringAsync());
        StringAssert.Contains(ignored.Headers.ETag!.ToString(), other);
        Assert.AreEqual(0, ignoredBody.RootElement.GetProperty("configurations").GetArrayLength());
    }

    [TestMethod]
    public async Task DeleteAndRecreateNeverReuseRevision()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("recreate-host");
        var route = $"/v1/machines/{id}/configurations/example";
        var before = await PutAsync(factory.Client, route, "{\"template\":{}}");
        using var deleted = await factory.Client.DeleteAsync(route);
        Assert.AreEqual(HttpStatusCode.NoContent, deleted.StatusCode);
        using var again = await factory.Client.DeleteAsync(route);
        Assert.AreEqual(HttpStatusCode.NotFound, again.StatusCode);
        var after = await PutAsync(factory.Client, route, "{\"template\":{}}");
        Assert.AreNotEqual(before.GetProperty("revision").GetString(), after.GetProperty("revision").GetString());
        Assert.AreEqual(3L, after.GetProperty("machineConfigurationRevision").GetInt64());
    }

    [TestMethod]
    [DataRow("{\"template\":[]}")]
    [DataRow("{\"template\":{\"a\":1,\"a\":2}}")]
    [DataRow("{\"template\":{\"format\":\"json\",\"content\":\"{}\"}}")]
    [DataRow("{\"template\":{\"s\":\"\\ud800\"}}")]
    [DataRow("{\"template\":{},\"tem\\u0070late\":{}}")]
    public async Task InvalidDocumentsAreRejected(string body)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("invalid-host");
        using var response = await factory.Client.PutAsync($"/v1/machines/{id}/configurations/example",
            new StringContent(body, Encoding.UTF8, "application/json"));
        Assert.AreEqual(HttpStatusCode.BadRequest, response.StatusCode);
        StringAssert.Contains(await response.Content.ReadAsStringAsync(), "invalid_configuration");
    }

    [TestMethod]
    [DataRow("*", true)]
    [DataRow("W/\"tag\"", true)]
    [DataRow("\"other\", \"tag\"", true)]
    [DataRow("\"comma,inside\", \"tag\"", true)]
    [DataRow(",\"tag\",", true)]
    [DataRow("\"tag\", invalid", false)]
    [DataRow("*, \"tag\"", false)]
    [DataRow("w/\"tag\"", true)]
    [DataRow("\"unterminated", false)]
    public void NativeConditionalHeaderParsingPreservesValidWeakComparison(string header, bool matches)
    {
        Assert.AreEqual(matches, AgentConfigurationsController.MatchesTag(header, "\"tag\""));
    }

    [TestMethod]
    [DataRow("{\"template\":{\"n\":1e2147483648}}")]
    [DataRow("{\"template\":{},\"parameters\":{\"n\":1e-2147483649}}")]
    public async Task UnsupportedNativeNumericComparisonsRejectWithoutChangingDesiredState(string body)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("numeric-bound");
        var route = $"/v1/machines/{id}/configurations/example";
        var before = await PutAsync(factory.Client, route, "{\"template\":{\"n\":1},\"parameters\":{}}");
        using var rejected = await factory.Client.PutAsync(route, new StringContent(body, Encoding.UTF8, "application/json"));
        Assert.AreEqual(HttpStatusCode.BadRequest, rejected.StatusCode);
        using var current = await factory.Client.GetAsync(route);
        current.EnsureSuccessStatusCode();
        using var after = JsonDocument.Parse(await current.Content.ReadAsStringAsync());
        Assert.AreEqual(before.GetProperty("revision").GetString(), after.RootElement.GetProperty("revision").GetString());
        Assert.AreEqual(before.GetProperty("machineConfigurationRevision").GetInt64(),
            after.RootElement.GetProperty("machineConfigurationRevision").GetInt64());
        Assert.IsTrue(JsonElement.DeepEquals(before.GetProperty("template"), after.RootElement.GetProperty("template")));
        Assert.IsTrue(JsonElement.DeepEquals(before.GetProperty("parameters"), after.RootElement.GetProperty("parameters")));
    }

    private static async Task<JsonElement> PutAsync(HttpClient client, string route, string body)
    {
        using var response = await client.PutAsync(route, new StringContent(body, Encoding.UTF8, "application/json"));
        response.EnsureSuccessStatusCode();
        using var json = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        return json.RootElement.Clone();
    }
}