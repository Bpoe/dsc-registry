namespace Dscregd.Tests;

using System.Net;
using System.Net.Http.Json;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class RoutingTests
{
    [TestMethod]
    [DataRow("OPTIONS", "/v1/machines", 405, "GET, HEAD, POST")]
    [DataRow("PUT", "/v1/machines/id", 405, "DELETE, GET, HEAD")]
    [DataRow("POST", "/v1/agent/configurations", 405, "GET, HEAD")]
    [DataRow("GET", "/v1/agent/register", 405, "POST")]
    [DataRow("GET", "/unknown", 404, "")]
    [DataRow("GET", "/v1%2Fmachines", 404, "")]
    public async Task RoutingPrecedesAuthentication(string method, string path, int status, string allow)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var client = factory.CreateClient();
        using var request = new HttpRequestMessage(new HttpMethod(method), path);
        using var response = await client.SendAsync(request);
        Assert.AreEqual(status, (int)response.StatusCode);
        Assert.IsNull(response.Content.Headers.ContentType);
        Assert.AreEqual(allow, string.Join(", ", response.Content.Headers.Allow));
    }

    [TestMethod]
    [DataRow("/v1/machines/")]
    [DataRow("/V1/MACHINES")]
    public async Task NativeRoutingAcceptsCasingAndTrailingSlashesWithoutBypassingAuthorization(string path)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var anonymous = factory.CreateClient();
        using var denied = await anonymous.GetAsync(path);
        Assert.AreEqual(HttpStatusCode.Unauthorized, denied.StatusCode);
        using var allowed = await factory.Client.GetAsync(path);
        Assert.AreEqual(HttpStatusCode.OK, allowed.StatusCode);
    }

    [TestMethod]
    public async Task EscapedSegmentsAreDecodedOnceAndAuthorizeBeforeValidation()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("routing-host");
        var escaped = id.Replace("-", "%2d", StringComparison.Ordinal);
        using var created = await factory.Client.PutAsJsonAsync($"/v1/machines/{escaped}/configurations/r%6futed",
            new { template = new { } });
        Assert.AreEqual(HttpStatusCode.Created, created.StatusCode);
        using var invalid = await factory.Client.GetAsync($"/v1/machines/{id}/configurations/a%2Fb");
        Assert.AreEqual(HttpStatusCode.BadRequest, invalid.StatusCode);
        StringAssert.Contains(await invalid.Content.ReadAsStringAsync(), "invalid_name");
        using var client = factory.CreateClient();
        using var unauthorized = await client.GetAsync("/v1/machines/not-an-id");
        Assert.AreEqual(HttpStatusCode.Unauthorized, unauthorized.StatusCode);
        using var head = await factory.Client.SendAsync(new HttpRequestMessage(HttpMethod.Head, "/v1/machines"));
        Assert.AreEqual(HttpStatusCode.OK, head.StatusCode);
        Assert.AreEqual(0, (await head.Content.ReadAsByteArrayAsync()).Length);
    }

    [TestMethod]
    public async Task HealthUsesStartupStateWithoutContinuousDependencyIo()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await factory.Client.GetAsync("/health/ready");
        Assert.AreEqual(HttpStatusCode.OK, response.StatusCode);
        StringAssert.Contains(await response.Content.ReadAsStringAsync(), "\"status\":\"ready\"");
    }
}