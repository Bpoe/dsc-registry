namespace Dscregd.Tests;

using System;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Data;
using Dscregd.Http;
using Dscregd.Reports;
using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Options;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class ReportsTests
{
    private readonly RegistryFactory factory = new();

    [TestInitialize]
    public async Task InitializeAsync()
    {
        await this.factory.InitializeAsync();
    }

    [TestCleanup]
    public async Task CleanupAsync()
    {
        await this.factory.DisposeAsync();
    }

    [TestMethod]
    public async Task OriginalBytesRetryConflictAndReplacement()
    {
        var machine = await this.CreateAsync();
        var revision = await this.ConfigureAsync(machine.Id, "docker", "{}");
        var uploadId = Guid.NewGuid().ToString();
        var body = ReportValidationTests.Wrap(ReportValidationTests.Success, revision, uploadId);
        using var firstResponse = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body);
        Assert.AreEqual(HttpStatusCode.Created, firstResponse.StatusCode);
        Assert.AreEqual("no-store", firstResponse.Headers.CacheControl?.ToString());
        var first = await ReadAsync<ReportMetadata>(firstResponse);
        var location = $"/v1/machines/{machine.Id}/reports/{first.Id}";
        Assert.AreEqual(location, firstResponse.Headers.Location?.OriginalString);
        Assert.AreEqual(machine.Id, first.MachineId);
        Assert.IsNull(first.InputHash);
        Assert.IsNull(first.ErrorKind);

        using var retry = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, " \n" + body + "\n");
        Assert.AreEqual(HttpStatusCode.OK, retry.StatusCode);
        Assert.AreEqual(first, await ReadAsync<ReportMetadata>(retry));
        Assert.AreEqual(location, retry.Headers.Location?.OriginalString);
        using var content = await this.SendAsync(HttpMethod.Get, location + "/content", this.factory.AdminKey);
        Assert.AreEqual("application/json", content.Content.Headers.ContentType?.MediaType);
        CollectionAssert.AreEqual(Encoding.UTF8.GetBytes(ReportValidationTests.Success), await content.Content.ReadAsByteArrayAsync());
        using var metadata = await this.SendAsync(HttpMethod.Get, location, this.factory.AdminKey);
        Assert.AreEqual(first, await ReadAsync<ReportMetadata>(metadata));
        using var rawMetadata = JsonDocument.Parse(await metadata.Content.ReadAsStringAsync());
        Assert.AreEqual(JsonValueKind.Null, rawMetadata.RootElement.GetProperty("inputHash").ValueKind);
        Assert.AreEqual(JsonValueKind.Null, rawMetadata.RootElement.GetProperty("errorKind").ValueKind);

        foreach (var conflictBody in new[]
        {
            body.Replace("\"schemaVersion\":1", "\"schemaVersion\": 1", StringComparison.Ordinal),
            body.Replace(revision, Guid.NewGuid().ToString(), StringComparison.Ordinal)
        })
        {
            using var conflict = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, conflictBody);
            await ErrorAsync(conflict, HttpStatusCode.Conflict, "upload_id_conflict");
        }

        var second = await this.UploadAsync(machine, "docker", revision);
        Assert.AreNotEqual(first.Id, second.Id);
        using var obsolete = await this.SendAsync(HttpMethod.Get, location + "/content", this.factory.AdminKey);
        await ErrorAsync(obsolete, HttpStatusCode.NotFound, "report_not_found");
        using var pageResponse = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports", this.factory.AdminKey);
        var page = await ReadAsync<ReportPage>(pageResponse);
        CollectionAssert.AreEqual(new[] { second }, page.Reports);
        using var pageJson = JsonDocument.Parse(await pageResponse.Content.ReadAsStringAsync());
        Assert.IsFalse(pageJson.RootElement.TryGetProperty("nextCursor", out _));
        using var scope = this.factory.Services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        var storedReports = await db.Reports.AsNoTracking().Where(report => report.MachineId == machine.Id).ToArrayAsync();
        Assert.AreEqual(1, storedReports.Length);
        var stored = storedReports[0];
        CollectionAssert.AreEqual(Encoding.UTF8.GetBytes(ReportValidationTests.Success),
            await File.ReadAllBytesAsync(Path.Combine(this.factory.Root, "reports", stored.ReportPath)));
        Assert.AreEqual(second.Id, stored.Id);
    }

    [TestMethod]
    public async Task StatusDistinguishesCurrentStaleUnreportedAndRemoved()
    {
        var machine = await this.CreateAsync();
        var root = $"/v1/machines/{machine.Id}";
        using var emptyResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        var empty = await ReadAsync<MachineStatus>(emptyResponse);
        Assert.AreEqual("never_reported", empty.Outcome);
        Assert.IsFalse(empty.HasReport);
        Assert.IsNull(empty.LastExecutionAt);
        Assert.IsNull(empty.LastReportReceivedAt);
        Assert.AreEqual(0, empty.Configurations.Length);

        var revision = await this.ConfigureAsync(machine.Id, "docker", "{}");
        await this.ConfigureAsync(machine.Id, "zeta", "{}");
        var first = await this.UploadAsync(machine, "docker", revision);
        using var currentResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        var current = await ReadAsync<MachineStatus>(currentResponse);
        Assert.AreEqual("unknown", current.Outcome);
        Assert.AreEqual(first, current.LatestReport);
        Assert.AreEqual(first, current.LatestReceivedReport);
        Assert.AreEqual(first.FinishedAt, current.LastExecutionAt);
        CollectionAssert.AreEqual(new[] { "docker", "zeta" }, current.Configurations.Select(configuration => configuration.Name).ToArray());
        CollectionAssert.AreEqual(new[] { "current", "unreported" }, current.Configurations.Select(configuration => configuration.RevisionState).ToArray());
        using var summaryResponse = await this.SendAsync(HttpMethod.Get, root + "/configurations/docker/status", this.factory.AdminKey);
        var summary = await ReadAsync<ConfigurationStatus>(summaryResponse);
        Assert.AreEqual(first.Id, summary.ReportId);
        Assert.IsTrue(summary.HasReport);

        var changedRevision = await this.ConfigureAsync(machine.Id, "docker", "{\"changed\":true}");
        Assert.AreNotEqual(revision, changedRevision);
        await this.UploadAsync(machine, "docker", revision, Failure("start", "failed"));
        using var staleResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        var stale = await ReadAsync<MachineStatus>(staleResponse);
        Assert.IsTrue(stale.HasStaleReports);
        Assert.IsNull(stale.LatestReport);
        Assert.IsNull(stale.LastExecutionAt);
        Assert.AreEqual("unknown", stale.Outcome);
        Assert.AreEqual("stale", stale.Configurations[0].RevisionState);
        Assert.AreEqual("failed", stale.LatestReceivedReport?.Outcome);
        foreach (var suffix in new[] { "/status", "/report" })
        {
            using var missing = await this.SendAsync(HttpMethod.Get, root + "/configurations/docker" + suffix, this.factory.AdminKey);
            await ErrorAsync(missing, HttpStatusCode.NotFound, "report_not_found");
        }

        await this.UploadAsync(machine, "docker", changedRevision, Failure("canceled", "canceled"));
        using var canceledResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        Assert.AreEqual("canceled", (await ReadAsync<MachineStatus>(canceledResponse)).Outcome);
        await this.UploadAsync(machine, "docker", changedRevision, Failure("dsc", "failed"));
        using var failedResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        Assert.AreEqual("failed", (await ReadAsync<MachineStatus>(failedResponse)).Outcome);

        using var deletion = await this.SendAsync(HttpMethod.Delete, root + "/configurations/docker", this.factory.AdminKey);
        Assert.AreEqual(HttpStatusCode.NoContent, deletion.StatusCode);
        var removedReceipt = await this.UploadAsync(machine, "docker", changedRevision);
        using var removedResponse = await this.SendAsync(HttpMethod.Get, root + "/status", this.factory.AdminKey);
        var removed = await ReadAsync<MachineStatus>(removedResponse);
        Assert.AreEqual("removed", removed.Configurations[0].RevisionState);
        Assert.IsNull(removed.Configurations[0].DesiredRevision);
        Assert.IsFalse(removed.HasStaleReports);
        Assert.AreEqual("unknown", removed.Outcome);
        Assert.AreEqual(removedReceipt, removed.LatestReceivedReport);
        using var removedNamed = await this.SendAsync(HttpMethod.Get, root + "/configurations/docker/report", this.factory.AdminKey);
        await ErrorAsync(removedNamed, HttpStatusCode.NotFound, "configuration_not_found");
    }

    [TestMethod]
    public async Task PaginationIsAcceptanceOrderedAndMachineScoped()
    {
        var machine = await this.CreateAsync();
        var other = await this.CreateAsync();
        var revision = Guid.NewGuid().ToString();
        var first = await this.UploadAsync(machine, "zeta", revision);
        var newest = await this.UploadAsync(machine, "alpha", revision);
        var root = $"/v1/machines/{machine.Id}/reports";
        using var pageResponse = await this.SendAsync(HttpMethod.Get, root + "?limit=1", this.factory.AdminKey);
        var page = await ReadAsync<ReportPage>(pageResponse);
        CollectionAssert.AreEqual(new[] { newest }, page.Reports);
        Assert.AreEqual(newest.Id, page.NextCursor);
        using var finalResponse = await this.SendAsync(HttpMethod.Get, root + "?limit=1&cursor=" + newest.Id, this.factory.AdminKey);
        var final = await ReadAsync<ReportPage>(finalResponse);
        CollectionAssert.AreEqual(new[] { first }, final.Reports);
        Assert.IsNull(final.NextCursor);
        using var crossMachine = await this.SendAsync(HttpMethod.Get,
            $"/v1/machines/{other.Id}/reports/{newest.Id}/content", this.factory.AdminKey);
        await ErrorAsync(crossMachine, HttpStatusCode.NotFound, "report_not_found");
        using var crossCursor = await this.SendAsync(HttpMethod.Get,
            $"/v1/machines/{other.Id}/reports?cursor={newest.Id}", this.factory.AdminKey);
        await ErrorAsync(crossCursor, HttpStatusCode.BadRequest, "invalid_pagination");
        await this.UploadAsync(machine, "alpha", revision);
        using var retiredCursor = await this.SendAsync(HttpMethod.Get, root + "?cursor=" + newest.Id, this.factory.AdminKey);
        await ErrorAsync(retiredCursor, HttpStatusCode.BadRequest, "invalid_pagination");
        using var machineRead = await this.SendAsync(HttpMethod.Get, root + "/" + first.Id, machine.Key);
        await ErrorAsync(machineRead, HttpStatusCode.Forbidden, "forbidden");
        using var suppliedMachine = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key,
            ReportValidationTests.Wrap(ReportValidationTests.Success).Replace("\"configurationName\":",
                "\"machineId\":\"" + other.Id + "\",\"configurationName\":", StringComparison.Ordinal));
        await ErrorAsync(suppliedMachine, HttpStatusCode.BadRequest, "invalid_report");
    }

    [TestMethod]
    public async Task ConcurrentRetainedRetriesProduceOneReceiptAndOneFile()
    {
        var machine = await this.CreateAsync();
        var body = ReportValidationTests.Wrap(ReportValidationTests.Success, Guid.NewGuid().ToString(), Guid.NewGuid().ToString());
        var responses = await Task.WhenAll(Enumerable.Range(0, 8)
            .Select(_ => this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body)));
        try
        {
            Assert.AreEqual(1, responses.Count(response => response.StatusCode == HttpStatusCode.Created));
            foreach (var response in responses)
            {
                Assert.IsTrue(response.StatusCode is HttpStatusCode.Created or HttpStatusCode.OK);
            }

            var metadata = await Task.WhenAll(responses.Select(ReadAsync<ReportMetadata>));
            Assert.AreEqual(1, metadata.Select(report => report.Id).Distinct().Count());
            foreach (var report in metadata)
            {
                Assert.AreEqual(metadata[0], report);
            }

            using var scope = this.factory.Services.CreateScope();
            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            Assert.AreEqual(1, await db.Reports.CountAsync(report => report.MachineId == machine.Id));
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
    [DataRow(false)]
    [DataRow(true)]
    public async Task StatementAndCommitFailurePreservePreviouslyPublishedReport(bool commitFailure)
    {
        var machine = await this.CreateAsync();
        var first = await this.UploadAsync(machine, "docker", Guid.NewGuid().ToString());
        var reportsRoot = Path.Combine(this.factory.Root, "reports");
        var before = Directory.GetFiles(reportsRoot).Length;
        await using var connection = new SqliteConnection($"Data Source={this.factory.DatabasePath};Foreign Keys=True");
        await connection.OpenAsync();
        var trigger = "report_fault_" + Guid.NewGuid().ToString("N");
        if (commitFailure)
        {
            await ExecuteAsync(connection, "CREATE TABLE IF NOT EXISTS report_fault_parent(id INTEGER PRIMARY KEY)");
            await ExecuteAsync(connection, "CREATE TABLE IF NOT EXISTS report_fault_child(id INTEGER REFERENCES report_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)");
        }

        var action = commitFailure ? "INSERT INTO report_fault_child VALUES (1);" : "SELECT RAISE(ABORT, 'injected failure');";
        await ExecuteAsync(connection,
            $"CREATE TRIGGER {trigger} AFTER INSERT ON reports WHEN NEW.machine_id = '{machine.Id}' BEGIN {action} END");
        try
        {
            using var failure = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key,
                ReportValidationTests.Wrap(ReportValidationTests.Success, first.ConfigurationRevision, Guid.NewGuid().ToString()));
            await ErrorAsync(failure, HttpStatusCode.ServiceUnavailable, "database_unavailable");
            using var previous = await this.SendAsync(HttpMethod.Get,
                $"/v1/machines/{machine.Id}/reports/{first.Id}/content", this.factory.AdminKey);
            Assert.AreEqual(HttpStatusCode.OK, previous.StatusCode);
            Assert.AreEqual(ReportValidationTests.Success, await previous.Content.ReadAsStringAsync());
            Assert.AreEqual(before + (commitFailure ? 1 : 0), Directory.GetFiles(reportsRoot).Length);
            using var page = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports", this.factory.AdminKey);
            CollectionAssert.AreEqual(new[] { first }, (await ReadAsync<ReportPage>(page)).Reports);
        }

        finally
        {
            await ExecuteAsync(connection, $"DROP TRIGGER {trigger}");
        }
    }

    [TestMethod]
    public async Task DigestBackfillSupportsExistingRelativeLayoutAndThenNeedsNoFilesystem()
    {
        var machine = await this.CreateAsync();
        var revision = Guid.NewGuid().ToString();
        var uploadId = Guid.NewGuid().ToString();
        var body = ReportValidationTests.Wrap(ReportValidationTests.Success, revision, uploadId);
        using var accepted = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body);
        var first = await ReadAsync<ReportMetadata>(accepted);
        var reportsRoot = Path.Combine(this.factory.Root, "reports");
        string legacyPath;
        using (var scope = this.factory.Services.CreateScope())
        {
            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            var stored = await db.Reports.SingleAsync(report => report.Id == first.Id);
            legacyPath = string.Join("/", machine.Id, "docker", uploadId + ".json");
            Directory.CreateDirectory(Path.Combine(reportsRoot, machine.Id, "docker"));
            File.Move(Path.Combine(reportsRoot, stored.ReportPath), Path.Combine(reportsRoot, machine.Id, "docker", uploadId + ".json"));
            stored.ReportPath = legacyPath;
            stored.ResultSha256 = null;
            await db.SaveChangesAsync();
        }

        using var backfill = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body);
        Assert.AreEqual(HttpStatusCode.OK, backfill.StatusCode);
        Assert.AreEqual(first, await ReadAsync<ReportMetadata>(backfill));
        using (var scope = this.factory.Services.CreateScope())
        {
            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            Assert.IsNotNull((await db.Reports.AsNoTracking().SingleAsync(report => report.Id == first.Id)).ResultSha256);
        }

        var offline = reportsRoot + "-offline";
        Directory.Move(reportsRoot, offline);
        try
        {
            using var retry = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body);
            Assert.AreEqual(HttpStatusCode.OK, retry.StatusCode);
            Assert.AreEqual(first, await ReadAsync<ReportMetadata>(retry));
            using var metadata = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports/{first.Id}", this.factory.AdminKey);
            Assert.AreEqual(HttpStatusCode.OK, metadata.StatusCode);
            using var content = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports/{first.Id}/content", this.factory.AdminKey);
            await ErrorAsync(content, HttpStatusCode.ServiceUnavailable, "report_storage_unavailable");
            using var failedWrite = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key,
                ReportValidationTests.Wrap(ReportValidationTests.Success, revision, Guid.NewGuid().ToString()));
            await ErrorAsync(failedWrite, HttpStatusCode.ServiceUnavailable, "report_storage_unavailable");
        }

        finally
        {
            Directory.Move(offline, reportsRoot);
        }

        Assert.AreEqual(ReportValidationTests.Success,
            await File.ReadAllTextAsync(Path.Combine(reportsRoot, legacyPath.Replace('/', Path.DirectorySeparatorChar))));
    }

    [TestMethod]
    public async Task StorageKeysAreConfinedAndMissingReferencedFilesAreErrors()
    {
        var machine = await this.CreateAsync();
        var first = await this.UploadAsync(machine, "docker", Guid.NewGuid().ToString());
        string path;
        using (var scope = this.factory.Services.CreateScope())
        {
            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            path = (await db.Reports.SingleAsync(report => report.Id == first.Id)).ReportPath;
            Assert.IsTrue(Guid.TryParseExact(Path.GetFileNameWithoutExtension(path), "D", out _));
            await db.Reports.Where(report => report.Id == first.Id)
                .ExecuteUpdateAsync(setters => setters.SetProperty(report => report.ReportPath, "../registry.db"));
        }

        using var unsafeRead = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports/{first.Id}/content", this.factory.AdminKey);
        await ErrorAsync(unsafeRead, HttpStatusCode.ServiceUnavailable, "report_storage_unavailable");
        using (var scope = this.factory.Services.CreateScope())
        {
            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            await db.Reports.Where(report => report.Id == first.Id)
                .ExecuteUpdateAsync(setters => setters.SetProperty(report => report.ReportPath, path));
        }

        File.Delete(Path.Combine(this.factory.Root, "reports", path));
        using var missing = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports/{first.Id}/content", this.factory.AdminKey);
        await ErrorAsync(missing, HttpStatusCode.ServiceUnavailable, "report_storage_unavailable");
        using var metadata = await this.SendAsync(HttpMethod.Get, $"/v1/machines/{machine.Id}/reports/{first.Id}", this.factory.AdminKey);
        Assert.AreEqual(HttpStatusCode.OK, metadata.StatusCode);
    }

    [TestMethod]
    public async Task ReportLimitsQueriesAuthorizationAndHeadAreEnforced()
    {
        var machine = await this.CreateAsync();
        var revision = Guid.NewGuid().ToString();
        var body = ReportValidationTests.Wrap(ReportValidationTests.Success, revision, Guid.NewGuid().ToString());
        var options = this.factory.Services.GetRequiredService<IOptions<RegistryOptions>>().Value;
        var previousRequestLimit = options.MaxRequestBytes;
        var previousReportLimit = options.MaxReportBytes;
        options.MaxRequestBytes = 128;
        options.MaxReportBytes = Encoding.UTF8.GetByteCount(body);
        try
        {
            using var accepted = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body);
            Assert.AreEqual(HttpStatusCode.Created, accepted.StatusCode);
            var metadata = await ReadAsync<ReportMetadata>(accepted);
            using var tooLarge = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body + " ");
            Assert.AreEqual(HttpStatusCode.RequestEntityTooLarge, tooLarge.StatusCode);
            using var query = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports?x=1", machine.Key, body);
            Assert.AreEqual(HttpStatusCode.OK, query.StatusCode);
            Assert.AreEqual(metadata, await ReadAsync<ReportMetadata>(query));
            using var unsupported = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key, body, "text/yaml");
            Assert.AreEqual(HttpStatusCode.UnsupportedMediaType, unsupported.StatusCode);
            using var wrongRole = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", this.factory.AdminKey, body);
            Assert.AreEqual(HttpStatusCode.Forbidden, wrongRole.StatusCode);
            using var invalidIdWrongRole = await this.SendAsync(HttpMethod.Get,
                "/v1/machines/not-a-uuid/reports/not-a-uuid", machine.Key);
            Assert.AreEqual(HttpStatusCode.Forbidden, invalidIdWrongRole.StatusCode);
            using var noCredential = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", "", "{}");
            Assert.AreEqual(HttpStatusCode.Unauthorized, noCredential.StatusCode);
            using var head = await this.SendAsync(HttpMethod.Head,
                $"/v1/machines/{machine.Id}/reports/{metadata.Id}/content", this.factory.AdminKey);
            Assert.AreEqual(HttpStatusCode.OK, head.StatusCode);
            Assert.AreEqual("no-store", head.Headers.CacheControl?.ToString());
            Assert.AreEqual(0, (await head.Content.ReadAsByteArrayAsync()).Length);
        }

        finally
        {
            options.MaxRequestBytes = previousRequestLimit;
            options.MaxReportBytes = previousReportLimit;
        }
    }

    private static string Failure(string kind, string outcome)
    {
        return $$$"""
            {"schemaVersion":1,"configuration":"docker","startedAt":"2026-10-18T12:00:00Z",
             "finishedAt":"2026-10-18T12:00:00.005Z","durationMs":5,"outcome":"{{{outcome}}}",
             "error":{"kind":"{{{kind}}}","message":"private diagnostic"}}
            """;
    }

    private static async Task<T> ReadAsync<T>(HttpResponseMessage response)
    {
        response.EnsureSuccessStatusCode();
        return (await response.Content.ReadFromJsonAsync<T>())!;
    }

    private static async Task ErrorAsync(HttpResponseMessage response, HttpStatusCode status, string code)
    {
        Assert.AreEqual(status, response.StatusCode);
        using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(code, body.RootElement.GetProperty("error").GetProperty("code").GetString());
    }

    private static async Task ExecuteAsync(SqliteConnection connection, string sql)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        await command.ExecuteNonQueryAsync();
    }

    private async Task<(string Id, string Key)> CreateAsync()
    {
        var id = await this.factory.CreateMachineAsync("reports-" + Guid.NewGuid());
        return (id, await this.factory.IssueMachineKeyAsync(id));
    }

    private async Task<string> ConfigureAsync(string machineId, string name, string template)
    {
        using var response = await this.SendAsync(HttpMethod.Put, $"/v1/machines/{machineId}/configurations/{name}",
            this.factory.AdminKey, "{\"template\":" + template + "}");
        response.EnsureSuccessStatusCode();
        using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        return body.RootElement.GetProperty("revision").GetString()!;
    }

    private async Task<ReportMetadata> UploadAsync((string Id, string Key) machine, string name, string revision,
        string? result = null)
    {
        using var response = await this.SendAsync(HttpMethod.Post, "/v1/agent/reports", machine.Key,
            ReportValidationTests.Wrap(result ?? ReportValidationTests.Success, revision, Guid.NewGuid().ToString(), name));
        Assert.AreEqual(HttpStatusCode.Created, response.StatusCode);
        return await ReadAsync<ReportMetadata>(response);
    }

    private async Task<HttpResponseMessage> SendAsync(HttpMethod method, string path, string credential,
        string? body = null, string mediaType = "application/json")
    {
        using var request = new HttpRequestMessage(method, path);
        request.Headers.Authorization = new AuthenticationHeaderValue("ApiKey", credential);
        if (body != null)
        {
            request.Content = new ByteArrayContent(Encoding.UTF8.GetBytes(body));
            request.Content.Headers.ContentType = MediaTypeHeaderValue.Parse(mediaType);
        }

        return await this.factory.Client.SendAsync(request, CancellationToken.None);
    }
}