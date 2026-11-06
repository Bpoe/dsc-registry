namespace Dscregd.Tests;

using System;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;
using Dscregd.Data;
using Dscregd.Services;
using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class MachinesTests
{
    [TestMethod]
    [DataRow("Docker-01", "docker-01")]
    [DataRow("e\u0301TAGE", "étage")]
    [DataRow("Straße", "straße")]
    [DataRow("ﬃ-HOST", "ﬃ-host")]
    public void LookupKeysUseStandardDotnetNormalizationAndInvariantLowercase(string value, string expected)
    {
        Assert.AreEqual(expected, MachineNames.CreateLookupKey(value));
    }

    [TestMethod]
    [DataRow("Docker-01", "DOCKER-01", "docker-01")]
    [DataRow("Étage", "e\u0301TAGE", "étage")]
    [DataRow("A\u030aring", "ÅRING", "åring")]
    [DataRow("ǰ", "j\u030C", "ǰ")]
    [DataRow("\U00010400-host", "\U00010428-HOST", "\U00010428-host")]
    [DataRow("literal_%_host+1", "LITERAL_%_HOST+1", "literal_%_host+1")]
    public async Task NormalizedNamesPreserveDisplayAndShareOneIndexedLookup(string original, string lookup, string folded)
    {
        Assert.AreEqual(folded, MachineNames.CreateLookupKey(original));
        Assert.AreEqual(folded, MachineNames.CreateLookupKey(lookup));
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync(original);
        using var response = await factory.Client.GetAsync("/v1/machines?name=" + Uri.EscapeDataString(lookup));
        Assert.AreEqual(HttpStatusCode.OK, response.StatusCode);
        using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(3, body.RootElement.EnumerateObject().Count());
        Assert.AreEqual(id, body.RootElement.GetProperty("id").GetString());
        Assert.AreEqual(original, body.RootElement.GetProperty("name").GetString());
        Assert.AreEqual(0L, body.RootElement.GetProperty("configurationRevision").GetInt64());
        using var conflict = await factory.Client.PostAsJsonAsync("/v1/machines", new { name = lookup });
        Assert.AreEqual(HttpStatusCode.Conflict, conflict.StatusCode);
        var conflictJson = await conflict.Content.ReadAsStringAsync();
        Assert.IsFalse(conflictJson.Contains(id, StringComparison.Ordinal));
        using var error = JsonDocument.Parse(conflictJson);
        Assert.AreEqual("machine_name_conflict", error.RootElement.GetProperty("error").GetProperty("code").GetString());
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath}");
        await connection.OpenAsync();
        using var command = connection.CreateCommand();
        command.CommandText = "SELECT name_folded FROM machines WHERE id = $id";
        command.Parameters.AddWithValue("$id", id);
        Assert.AreEqual(folded, await command.ExecuteScalarAsync());
    }

    [TestMethod]
    public async Task NamesDoNotUseCompatibilityNormalizationAccentRemovalOrPatterns()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        foreach (var name in new[] { "A", "Ａ", "resume", "résumé", "I", "İ", "ı", "literal_%+1", "�", "Straße", "STRASSE" })
        {
            var id = await factory.CreateMachineAsync(name);
            using var found = await factory.Client.GetAsync("/v1/machines?name=" + Uri.EscapeDataString(name));
            Assert.AreEqual(HttpStatusCode.OK, found.StatusCode);
            using var body = JsonDocument.Parse(await found.Content.ReadAsStringAsync());
            Assert.AreEqual(id, body.RootElement.GetProperty("id").GetString());
        }

        foreach (var name in new[] { "res", "r%s%", "literal_XX+1", "literal_%", "missing" })
        {
            using var miss = await factory.Client.GetAsync("/v1/machines?name=" + Uri.EscapeDataString(name));
            Assert.AreEqual(HttpStatusCode.NotFound, miss.StatusCode);
        }
    }

    [TestMethod]
    [DataRow("{}", "invalid_name")]
    [DataRow("null", "invalid_json")]
    [DataRow("[]", "invalid_json")]
    [DataRow("""{"name":null}""", "invalid_json")]
    [DataRow("""{"name":1}""", "invalid_json")]
    [DataRow("""{"name":""}""", "invalid_name")]
    [DataRow("""{"Name":"host"}""", "invalid_json")]
    [DataRow("""{"name":" leading"}""", "invalid_name")]
    [DataRow("""{"name":"trailing "}""", "invalid_name")]
    [DataRow("""{"name":"bad\nname"}""", "invalid_name")]
    [DataRow("""{"name":"one","name":"two"}""", "invalid_json")]
    [DataRow("""{"name":"one","n\u0061me":"two"}""", "invalid_json")]
    [DataRow("""{"name":"host","id":"supplied"}""", "invalid_json")]
    [DataRow("""{"name":"host"} {}""", "invalid_json")]
    public async Task MachineBodiesRejectUnknownDuplicateAndNonStringFields(string json, string code)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var content = new StringContent(json, Encoding.UTF8, "application/json");
        using var response = await factory.Client.PostAsync("/v1/machines", content);
        Assert.AreEqual(HttpStatusCode.BadRequest, response.StatusCode);
        using var error = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(code, error.RootElement.GetProperty("error").GetProperty("code").GetString());
        using var list = await factory.Client.GetAsync("/v1/machines");
        using var body = JsonDocument.Parse(await list.Content.ReadAsStringAsync());
        Assert.AreEqual(0, body.RootElement.GetProperty("machines").GetArrayLength());
    }

    [TestMethod]
    public async Task NameAndDescriptionLimitsCountUtf8BytesAndAllowEmptyDescriptions()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync(new string('é', 128));
        using var invalid = await factory.Client.PostAsJsonAsync("/v1/machines", new { name = new string('é', 129) });
        Assert.AreEqual(HttpStatusCode.BadRequest, invalid.StatusCode);
        foreach (var description in new[] { "", new string('é', 512) })
        {
            using var valid = await factory.Client.PostAsJsonAsync($"/v1/machines/{id}/api-keys", new { description });
            Assert.AreEqual(HttpStatusCode.Created, valid.StatusCode);
        }

        foreach (var description in new[] { " leading", "trailing ", "line\nbreak", new string('é', 513) })
        {
            using var denied = await factory.Client.PostAsJsonAsync($"/v1/machines/{id}/api-keys", new { description });
            Assert.AreEqual(HttpStatusCode.BadRequest, denied.StatusCode);
            using var error = JsonDocument.Parse(await denied.Content.ReadAsStringAsync());
            Assert.AreEqual("invalid_description", error.RootElement.GetProperty("error").GetProperty("code").GetString());
        }
    }

    [TestMethod]
    [DataRow("?name=", "invalid_name")]
    [DataRow("?name=%00", "invalid_name")]
    [DataRow("?name=+host", "invalid_name")]
    [DataRow("?limit=%xx", "invalid_pagination")]
    [DataRow("?limit=0", "invalid_pagination")]
    [DataRow("?limit=1001", "invalid_pagination")]
    [DataRow("?limit=x", "invalid_pagination")]
    public async Task NativeQueryBindingRejectsInvalidNamesAndOutOfRangeLimits(string query, string code)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await CredentialsTests.SendAsync(factory, HttpMethod.Get,
            "/v1/machines" + query, "ApiKey " + factory.AdminKey);
        Assert.AreEqual(HttpStatusCode.BadRequest, response.StatusCode);
        using var error = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(code, error.RootElement.GetProperty("error").GetProperty("code").GetString());
    }

    [TestMethod]
    [DataRow("?Name=host")]
    [DataRow("?name=host&name=ignored")]
    [DataRow("?name=host&limit=1")]
    [DataRow("?name=host&unknown=1")]
    public async Task NativeQueryBindingUsesCaselessKeysFirstValuesAndIgnoresUnboundFields(string query)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("host");
        using var response = await factory.Client.GetAsync("/v1/machines" + query);
        Assert.AreEqual(HttpStatusCode.OK, response.StatusCode);
        using var result = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(id, result.RootElement.GetProperty("id").GetString());
    }

    [TestMethod]
    [DataRow("?name=host;other")]
    [DataRow("?name=%ff")]
    [DataRow("?name=%xx")]
    public async Task NativeQueryDecoderDoesNotReimplementGoEscapeValidation(string query)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await factory.Client.GetAsync("/v1/machines" + query);
        Assert.AreEqual(HttpStatusCode.NotFound, response.StatusCode);
    }

    [TestMethod]
    public async Task ExistingGoLookupKeysAreNotSilentlyRewrittenAndUuidAccessStillWorks()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("Straße");
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath}");
        await connection.OpenAsync();
        using var command = connection.CreateCommand();
        command.CommandText = "UPDATE machines SET name_folded = 'strasse' WHERE id = $id";
        command.Parameters.AddWithValue("$id", id);
        await command.ExecuteNonQueryAsync();
        using var byId = await factory.Client.GetAsync("/v1/machines/" + id.ToUpperInvariant());
        Assert.AreEqual(HttpStatusCode.OK, byId.StatusCode);
        using var oldKey = await factory.Client.GetAsync("/v1/machines?name=STRASSE");
        Assert.AreEqual(HttpStatusCode.OK, oldKey.StatusCode);
        using var differentKey = await factory.Client.GetAsync("/v1/machines?name=Stra%C3%9Fe");
        Assert.AreEqual(HttpStatusCode.NotFound, differentKey.StatusCode);
        command.CommandText = "SELECT name_folded FROM machines WHERE id = $id";
        Assert.AreEqual("strasse", await command.ExecuteScalarAsync());
        var key = await factory.IssueMachineKeyAsync(id);
        using var agent = await CredentialsTests.SendAsync(factory, HttpMethod.Get,
            "/v1/agent/configurations", "ApiKey " + key);
        Assert.AreEqual(HttpStatusCode.OK, agent.StatusCode);
    }

    [TestMethod]
    public async Task InvalidUnicodeIsRejectedAtNativeJsonAndServiceBoundariesWithoutInsertingAMachine()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var content = new StringContent("""{"name":"host\ud800"}""", Encoding.UTF8, "application/json");
        using var response = await factory.Client.PostAsync("/v1/machines", content);
        Assert.AreEqual(HttpStatusCode.BadRequest, response.StatusCode);
        using var error = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual("invalid_json", error.RootElement.GetProperty("error").GetProperty("code").GetString());
        using var scope = factory.Services.CreateScope();
        var machines = scope.ServiceProvider.GetRequiredService<MachineService>();
        var rejected = await Assert.ThrowsExactlyAsync<DomainException>(() => machines.CreateAsync("host\ud800", CancellationToken.None));
        Assert.AreEqual("invalid_name", rejected.Code);
        Assert.AreEqual(0, await scope.ServiceProvider.GetRequiredService<RegistryDbContext>().Machines.CountAsync());
    }

    [TestMethod]
    public async Task MachineAndKeyPagesAreAscendingAndOmitFinalCursor()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var first = await factory.CreateMachineAsync("first");
        var second = await factory.CreateMachineAsync("second");
        var expected = new[] { first, second }.Order(StringComparer.Ordinal).ToArray();
        using var page = await factory.Client.GetAsync("/v1/machines?limit=1");
        using var pageBody = JsonDocument.Parse(await page.Content.ReadAsStringAsync());
        var cursor = pageBody.RootElement.GetProperty("nextCursor").GetString()!;
        Assert.AreEqual(expected[0], cursor);
        Assert.AreEqual(cursor, pageBody.RootElement.GetProperty("machines")[0].GetProperty("id").GetString());
        using var final = await factory.Client.GetAsync("/v1/machines?limit=1&cursor=" + cursor);
        using var finalBody = JsonDocument.Parse(await final.Content.ReadAsStringAsync());
        Assert.AreEqual(expected[1], finalBody.RootElement.GetProperty("machines")[0].GetProperty("id").GetString());
        Assert.IsFalse(finalBody.RootElement.TryGetProperty("nextCursor", out _));
        using var empty = await factory.Client.GetAsync("/v1/machines?cursor=" + expected[1]);
        using var emptyBody = JsonDocument.Parse(await empty.Content.ReadAsStringAsync());
        Assert.AreEqual(0, emptyBody.RootElement.GetProperty("machines").GetArrayLength());
        var firstKey = await factory.IssueMachineKeyAsync(first);
        var secondKey = await factory.IssueMachineKeyAsync(first);
        var expectedKeys = new[] { firstKey[5..29], secondKey[5..29] }.Order(StringComparer.Ordinal).ToArray();
        using var keyPage = await factory.Client.GetAsync($"/v1/machines/{first}/api-keys?limit=1");
        using var keyPageBody = JsonDocument.Parse(await keyPage.Content.ReadAsStringAsync());
        Assert.AreEqual(expectedKeys[0], keyPageBody.RootElement.GetProperty("nextCursor").GetString());
        using var keyFinal = await factory.Client.GetAsync(
            $"/v1/machines/{first}/api-keys?limit=1&cursor={expectedKeys[0]}");
        using var keyFinalBody = JsonDocument.Parse(await keyFinal.Content.ReadAsStringAsync());
        Assert.AreEqual(expectedKeys[1], keyFinalBody.RootElement.GetProperty("keys")[0].GetProperty("keyId").GetString());
        Assert.IsFalse(keyFinalBody.RootElement.TryGetProperty("nextCursor", out _));
    }

    [TestMethod]
    public async Task HeadAndIgnoredNonListQueryStringsPreserveGoBehavior()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var create = await factory.Client.PostAsJsonAsync("/v1/machines?ignored=1", new { name = "host" });
        Assert.AreEqual(HttpStatusCode.Created, create.StatusCode);
        using var created = JsonDocument.Parse(await create.Content.ReadAsStringAsync());
        var id = created.RootElement.GetProperty("id").GetString()!;
        foreach (var path in new[] { "/v1/machines", $"/v1/machines/{id}?ignored=1", $"/v1/machines/{id}/api-keys" })
        {
            using var request = new HttpRequestMessage(HttpMethod.Head, path);
            using var head = await factory.Client.SendAsync(request);
            Assert.AreEqual(HttpStatusCode.OK, head.StatusCode);
            Assert.AreEqual("no-store", head.Headers.CacheControl!.ToString());
            Assert.AreEqual("", await head.Content.ReadAsStringAsync());
        }

        using var delete = await factory.Client.DeleteAsync($"/v1/machines/{id}?ignored=1");
        Assert.AreEqual(HttpStatusCode.NoContent, delete.StatusCode);
    }

    [TestMethod]
    public async Task ConcurrentEquivalentNamesProduceOneWinnerAndNoOrphanKeys()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var names = new[] { "Café-Host", "CAFE\u0301-HOST", "café-host", "cafe\u0301-host" };
        var responses = await Task.WhenAll(names.Select(name =>
            factory.Client.PostAsJsonAsync("/v1/machines", new { name })));
        try
        {
            Assert.AreEqual(1, responses.Count(response => response.StatusCode == HttpStatusCode.Created));
            Assert.AreEqual(3, responses.Count(response => response.StatusCode == HttpStatusCode.Conflict));
            using var list = await factory.Client.GetAsync("/v1/machines");
            using var body = JsonDocument.Parse(await list.Content.ReadAsStringAsync());
            Assert.AreEqual(1, body.RootElement.GetProperty("machines").GetArrayLength());
        }
        finally
        {
            foreach (var response in responses)
            {
                response.Dispose();
            }
        }
    }

    [TestMethod]
    public async Task DeletionCascadesSqlThenRetiresReportsAndMakesKeysUnusable()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("host");
        var key = await factory.IssueMachineKeyAsync(id);
        var reportId = Guid.NewGuid().ToString();
        var relative = reportId + ".json";
        var path = Path.Combine(factory.Root, "reports", relative);
        await File.WriteAllTextAsync(path, "{}");
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath};Foreign Keys=True");
        await connection.OpenAsync();
        using var command = connection.CreateCommand();
        command.CommandText = """
            INSERT INTO reports
            (id,machine_id,sequence,name,configuration_revision,received_at,started_at,finished_at,outcome,report_path,upload_id)
            VALUES ($id,$machine,1,'example',$revision,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',
            '2026-01-01T00:00:00Z','succeeded',$path,$upload)
            """;
        command.Parameters.AddWithValue("$id", reportId);
        command.Parameters.AddWithValue("$machine", id);
        command.Parameters.AddWithValue("$revision", Guid.NewGuid().ToString());
        command.Parameters.AddWithValue("$path", relative);
        command.Parameters.AddWithValue("$upload", Guid.NewGuid().ToString());
        await command.ExecuteNonQueryAsync();
        using var deleted = await factory.Client.DeleteAsync("/v1/machines/" + id);
        Assert.AreEqual(HttpStatusCode.NoContent, deleted.StatusCode);
        Assert.IsFalse(File.Exists(path));
        command.CommandText = "SELECT COUNT(*) FROM api_keys WHERE owner_type = 'machine' AND owner_id = $machine";
        Assert.AreEqual(0L, await command.ExecuteScalarAsync());
        command.CommandText = "SELECT COUNT(*) FROM reports WHERE machine_id = $machine";
        Assert.AreEqual(0L, await command.ExecuteScalarAsync());
        using var request = new HttpRequestMessage(HttpMethod.Get, "/v1/agent/configurations");
        request.Headers.TryAddWithoutValidation("Authorization", "ApiKey " + key);
        using var revoked = await factory.Client.SendAsync(request);
        Assert.AreEqual(HttpStatusCode.Unauthorized, revoked.StatusCode);
        using var missing = await factory.Client.GetAsync("/v1/machines/" + id);
        Assert.AreEqual(HttpStatusCode.NotFound, missing.StatusCode);
        var recreated = await factory.CreateMachineAsync("HOST");
        Assert.AreNotEqual(id, recreated);
    }
}