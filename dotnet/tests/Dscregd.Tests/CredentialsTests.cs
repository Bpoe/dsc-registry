namespace Dscregd.Tests;

using System;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Authentication;
using Dscregd.Data;
using Dscregd.Services;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Http.Features;
using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class CredentialsTests
{
    [TestMethod]
    public async Task DirectServiceAndAuthenticationBoundariesPreserveDatabaseSemantics()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var scope = factory.Services.CreateScope();
        var machines = scope.ServiceProvider.GetRequiredService<MachineService>();
        var credentials = scope.ServiceProvider.GetRequiredService<CredentialService>();
        var machine = await machines.CreateAsync("Straße", CancellationToken.None);
        Assert.AreEqual(machine, await machines.FindByNameAsync("STRAßE", CancellationToken.None));
        var key = await credentials.IssueAsync("machine", machine.Id, "agent", CancellationToken.None);
        var keys = await credentials.ListAsync("machine", machine.Id, 1, "", CancellationToken.None);
        Assert.AreEqual(1, keys.Keys.Count);
        Assert.IsNull(keys.NextCursor);
        Assert.AreEqual(key.KeyId, keys.Keys[0].KeyId);
        Assert.AreEqual(machine.Id, keys.Keys[0].OwnerId);
        var page = await machines.ListAsync(1, "", CancellationToken.None);
        Assert.AreEqual(1, page.Machines.Count);
        Assert.IsNull(page.NextCursor);
        var token = await credentials.CreateRegistrationTokenAsync(3600, CancellationToken.None);
        Assert.IsTrue(await credentials.AuthenticateRegistrationAsync(token.Token, CancellationToken.None));
        Assert.IsFalse(await credentials.AuthenticateRegistrationAsync(factory.AdminKey, CancellationToken.None));
        Assert.IsNull(await credentials.AuthenticateAsync(token.Token, CancellationToken.None));
        using (var authenticationScope = factory.Services.CreateScope())
        {
            var context = new DefaultHttpContext { RequestServices = authenticationScope.ServiceProvider };
            context.Request.Headers.Authorization = "ApiKey " + key.Key;
            var authentication = authenticationScope.ServiceProvider.GetRequiredService<IAuthenticationService>();
            var result = await authentication.AuthenticateAsync(context, RegistryAuthentication.ApiKeyScheme);
            Assert.IsTrue(result.Succeeded);
            Assert.AreEqual(machine.Id, result.Principal!.FindFirst(System.Security.Claims.ClaimTypes.NameIdentifier)!.Value);
            Assert.IsTrue(result.Principal.IsInRole("machine"));
            var authorization = authenticationScope.ServiceProvider.GetRequiredService<IAuthorizationService>();
            Assert.IsTrue((await authorization.AuthorizeAsync(result.Principal, null, "Machine")).Succeeded);
            Assert.IsFalse((await authorization.AuthorizeAsync(result.Principal, null, "Admin")).Succeeded);
        }

        using (var registrationScope = factory.Services.CreateScope())
        {
            var context = new DefaultHttpContext { RequestServices = registrationScope.ServiceProvider };
            context.Request.Headers.Authorization = "Bearer " + token.Token;
            var authentication = registrationScope.ServiceProvider.GetRequiredService<IAuthenticationService>();
            var result = await authentication.AuthenticateAsync(context, RegistryAuthentication.RegistrationScheme);
            Assert.IsTrue(result.Succeeded);
            Assert.IsTrue(result.Principal!.IsInRole("registration"));
            Assert.IsFalse((await authentication.AuthenticateAsync(context, RegistryAuthentication.ApiKeyScheme)).Succeeded);
        }

        var registered = await machines.RegisterAsync("enrolled", CancellationToken.None);
        Assert.AreEqual(registered.Id,
            (await credentials.AuthenticateAsync(registered.ApiKey, CancellationToken.None))!.OwnerId);
        await credentials.RevokeAsync("machine", machine.Id, key.KeyId, CancellationToken.None);
        Assert.IsNull(await credentials.AuthenticateAsync(key.Key, CancellationToken.None));
        var revoked = await credentials.ListAsync("machine", machine.Id, 100, "", CancellationToken.None);
        Assert.IsNotNull(revoked.Keys[0].RevokedAt);
        await machines.DeleteAsync(registered.Id, CancellationToken.None);
        Assert.IsNull(await credentials.AuthenticateAsync(registered.ApiKey, CancellationToken.None));
    }

    [TestMethod]
    public async Task NativeAuthenticationSeparatesRolesAndValidatesBeforeResourceIds()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("host");
        var machineKey = await factory.IssueMachineKeyAsync(id);
        foreach (var path in new[] { "/v1/machines", "/v1/machines/not-a-uuid", $"/v1/machines/{id}/api-keys" })
        {
            using var forbidden = await SendAsync(factory, HttpMethod.Get, path, "ApiKey " + machineKey);
            await AssertErrorAsync(forbidden, HttpStatusCode.Forbidden, "forbidden");
        }

        var authorizationCase = 0;
        foreach (var authorization in new[]
        {
            "", "Bearer " + factory.AdminKey, "ApiKey invalid",
            "ApiKey dsca_" + machineKey[5..], "ApiKey dscm_" + factory.AdminKey[5..]
        })
        {
            using var response = await SendAsync(factory, HttpMethod.Get, "/v1/machines/not-a-uuid", authorization);
            Assert.IsTrue(response.StatusCode == HttpStatusCode.Unauthorized,
                $"Authorization case {authorizationCase} returned {response.StatusCode}");
            await AssertErrorAsync(response, HttpStatusCode.Unauthorized, "unauthorized");
            var challenges = response.Headers.WwwAuthenticate.ToArray();
            Assert.AreEqual(1, challenges.Length);
            Assert.AreEqual("ApiKey", challenges[0].Scheme);
            authorizationCase++;
        }

        using var request = new HttpRequestMessage(HttpMethod.Get, "/v1/machines");
        request.Headers.TryAddWithoutValidation("Authorization",
            new[] { "ApiKey " + factory.AdminKey, "ApiKey " + factory.AdminKey });
        using var multiple = await factory.Client.SendAsync(request);
        await AssertErrorAsync(multiple, HttpStatusCode.Unauthorized, "unauthorized");
        using var adminOnAgent = await SendAsync(factory, HttpMethod.Get, "/v1/agent/configurations",
            "ApiKey " + factory.AdminKey);
        await AssertErrorAsync(adminOnAgent, HttpStatusCode.Forbidden, "forbidden");
        using var lowercase = await SendAsync(factory, HttpMethod.Get, "/v1/machines", "apikey " + factory.AdminKey);
        Assert.AreEqual(HttpStatusCode.OK, lowercase.StatusCode);
    }

    [TestMethod]
    public async Task KeyMetadataRotationRevocationAndOwnershipRemainGoCompatible()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var first = await factory.CreateMachineAsync("first");
        var second = await factory.CreateMachineAsync("second");
        using var issued = await SendAsync(factory, HttpMethod.Post, $"/v1/machines/{first}/api-keys",
            "ApiKey " + factory.AdminKey, """{"description":"agent credential"}""");
        Assert.AreEqual(HttpStatusCode.Created, issued.StatusCode);
        using var keyBody = JsonDocument.Parse(await issued.Content.ReadAsStringAsync());
        var keyId = keyBody.RootElement.GetProperty("keyId").GetString()!;
        var key = keyBody.RootElement.GetProperty("key").GetString()!;
        Assert.AreEqual(73, key.Length);
        Assert.IsTrue(CredentialService.ValidKeyId(keyId));
        Assert.IsTrue(CredentialFormat.TryParse(key, out var role, out var parsedId));
        Assert.AreEqual("machine", role);
        Assert.AreEqual(keyId, parsedId);
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath}");
        await connection.OpenAsync();
        using var digestQuery = connection.CreateCommand();
        digestQuery.CommandText = "SELECT secret_hash FROM api_keys WHERE key_id = $id";
        digestQuery.Parameters.AddWithValue("$id", keyId);
        CollectionAssert.AreEqual(CredentialFormat.Digest(key), (byte[])(await digestQuery.ExecuteScalarAsync())!);
        using var crossOwner = await SendAsync(factory, HttpMethod.Delete, $"/v1/machines/{second}/api-keys/{keyId}",
            "ApiKey " + factory.AdminKey);
        await AssertErrorAsync(crossOwner, HttpStatusCode.NotFound, "machine_not_found");
        using var metadata = await factory.Client.GetAsync($"/v1/machines/{first}/api-keys");
        var metadataJson = await metadata.Content.ReadAsStringAsync();
        Assert.IsFalse(metadataJson.Contains(key, StringComparison.Ordinal));
        Assert.IsFalse(metadataJson.Contains("secretHash", StringComparison.Ordinal));
        using var metadataBody = JsonDocument.Parse(metadataJson);
        var entry = metadataBody.RootElement.GetProperty("keys")[0];
        Assert.AreEqual(JsonValueKind.Null, entry.GetProperty("revokedAt").ValueKind);
        Assert.AreEqual("agent credential", entry.GetProperty("description").GetString());
        for (var attempt = 0; attempt < 2; attempt++)
        {
            using var revoke = await factory.Client.DeleteAsync($"/v1/machines/{first}/api-keys/{keyId}");
            Assert.AreEqual(HttpStatusCode.NoContent, revoke.StatusCode);
        }

        using var revoked = await SendAsync(factory, HttpMethod.Get, "/v1/agent/configurations", "ApiKey " + key);
        await AssertErrorAsync(revoked, HttpStatusCode.Unauthorized, "unauthorized");
        using var rotation = await SendAsync(factory, HttpMethod.Post, $"/v1/admins/{factory.AdminId}/api-keys",
            "ApiKey " + factory.AdminKey, """{"description":"rotation"}""");
        Assert.AreEqual(HttpStatusCode.Created, rotation.StatusCode);
        using var rotationBody = JsonDocument.Parse(await rotation.Content.ReadAsStringAsync());
        var replacement = rotationBody.RootElement.GetProperty("key").GetString()!;
        StringAssert.StartsWith(replacement, "dsca_");
        var oldId = factory.AdminKey[5..29];
        using var revokeOld = await SendAsync(factory, HttpMethod.Delete,
            $"/v1/admins/{factory.AdminId}/api-keys/{oldId}", "ApiKey " + replacement);
        Assert.AreEqual(HttpStatusCode.NoContent, revokeOld.StatusCode);
        using var old = await SendAsync(factory, HttpMethod.Get, "/v1/machines", "ApiKey " + factory.AdminKey);
        await AssertErrorAsync(old, HttpStatusCode.Unauthorized, "unauthorized");
        using var current = await SendAsync(factory, HttpMethod.Get, $"/v1/machines/{first}", "ApiKey " + replacement);
        Assert.AreEqual(HttpStatusCode.OK, current.StatusCode);
        using var currentBody = JsonDocument.Parse(await current.Content.ReadAsStringAsync());
        Assert.AreEqual(0L, currentBody.RootElement.GetProperty("configurationRevision").GetInt64());
    }

    [TestMethod]
    [DataRow("ApiKey  ")]
    [DataRow("ApiKey\t")]
    [DataRow("apikey ")]
    public async Task AuthorizationHeaderUsesNativeSchemeAndWhitespaceParsing(string prefix)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await SendAsync(factory, HttpMethod.Get, "/v1/machines", prefix + factory.AdminKey);
        Assert.AreEqual(HttpStatusCode.OK, response.StatusCode);
    }

    [TestMethod]
    public async Task RegistrationTokensAreReusableDigestOnlyAndExpireWithoutRevokingIssuedKeys()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var existing = await factory.CreateMachineAsync("existing");
        var existingKey = await factory.IssueMachineKeyAsync(existing);
        using var created = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + factory.AdminKey, "{}");
        Assert.AreEqual(HttpStatusCode.Created, created.StatusCode);
        using var tokenBody = JsonDocument.Parse(await created.Content.ReadAsStringAsync());
        var token = tokenBody.RootElement.GetProperty("token").GetString()!;
        StringAssert.StartsWith(token, "dscr_");
        Assert.AreEqual(48, token.Length);
        var expiration = DateTimeOffset.Parse(tokenBody.RootElement.GetProperty("expiresAt").GetString()!);
        var remainingSeconds = (expiration - DateTimeOffset.UtcNow).TotalSeconds;
        Assert.IsTrue(remainingSeconds is >= 3590 and <= 3600);
        var authorizationCase = 0;
        foreach (var authorization in new[]
        {
            "", "Bearer invalid", "ApiKey " + token, "ApiKey " + factory.AdminKey,
            "Bearer " + factory.AdminKey, "Bearer " + existingKey
        })
        {
            using var denied = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register", authorization, "not json");
            Assert.IsTrue(denied.StatusCode == HttpStatusCode.Unauthorized,
                $"Registration authorization case {authorizationCase} returned {denied.StatusCode}");
            await AssertErrorAsync(denied, HttpStatusCode.Unauthorized, "unauthorized");
            var challenges = denied.Headers.WwwAuthenticate.ToArray();
            Assert.AreEqual(1, challenges.Length);
            Assert.AreEqual("Bearer", challenges[0].Scheme);
            authorizationCase++;
        }

        foreach (var scheme in new[] { "ApiKey ", "Bearer " })
        {
            using var denied = await SendAsync(factory, HttpMethod.Get, "/v1/machines", scheme + token);
            await AssertErrorAsync(denied, HttpStatusCode.Unauthorized, "unauthorized");
            var challenges = denied.Headers.WwwAuthenticate.ToArray();
            Assert.AreEqual(1, challenges.Length);
            Assert.AreEqual("ApiKey", challenges[0].Scheme);
        }

        using var wrongRole = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + existingKey, "{}");
        await AssertErrorAsync(wrongRole, HttpStatusCode.Forbidden, "forbidden");
        var issuedKey = "";
        foreach (var name in new[] { "Straße", "second" })
        {
            using var registration = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register",
                "bearer " + token, JsonSerializer.Serialize(new { name }));
            Assert.AreEqual(HttpStatusCode.Created, registration.StatusCode);
            using var registrationBody = JsonDocument.Parse(await registration.Content.ReadAsStringAsync());
            var id = registrationBody.RootElement.GetProperty("id").GetString()!;
            Assert.AreEqual(3, registrationBody.RootElement.EnumerateObject().Count());
            Assert.AreEqual("/v1/machines/" + id, registration.Headers.Location!.OriginalString);
            issuedKey = registrationBody.RootElement.GetProperty("apiKey").GetString()!;
            using var agent = await SendAsync(factory, HttpMethod.Get, "/v1/agent/configurations", "ApiKey " + issuedKey);
            Assert.AreEqual(HttpStatusCode.OK, agent.StatusCode);
        }

        using var conflict = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register",
            "Bearer " + token, """{"name":"STRAßE"}""");
        await AssertErrorAsync(conflict, HttpStatusCode.Conflict, "machine_name_conflict");
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath}");
        await connection.OpenAsync();
        using var command = connection.CreateCommand();
        command.CommandText = "SELECT token_hash FROM registration_tokens";
        CollectionAssert.AreEqual(CredentialFormat.Digest(token), (byte[])(await command.ExecuteScalarAsync())!);
        command.CommandText = "UPDATE registration_tokens SET expires_at = '2000-01-01T00:00:00Z'";
        await command.ExecuteNonQueryAsync();
        using var expired = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register",
            "Bearer " + token, """{"name":"third"}""");
        await AssertErrorAsync(expired, HttpStatusCode.Unauthorized, "unauthorized");
        using var afterExpiry = await SendAsync(factory, HttpMethod.Get, "/v1/agent/configurations", "ApiKey " + issuedKey);
        Assert.AreEqual(HttpStatusCode.OK, afterExpiry.StatusCode);
    }

    [TestMethod]
    [DataRow("""{"ttlSeconds":0}""", "invalid_ttl")]
    [DataRow("""{"ttlSeconds":-1}""", "invalid_ttl")]
    [DataRow("""{"ttlSeconds":86401}""", "invalid_ttl")]
    [DataRow("""{"ttlSeconds":9223372036854775808}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":1.0}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":1e3}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":null}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":"3600"}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":true}""", "invalid_json")]
    [DataRow("""{"ttlSeconds":1,"ttlSeconds":2}""", "invalid_json")]
    [DataRow("""{"TTLSeconds":10}""", "invalid_json")]
    [DataRow("null", "invalid_json")]
    [DataRow("{} {}", "invalid_json")]
    public async Task InvalidTokenCreationCannotMutateState(string json, string code)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + factory.AdminKey, json);
        await AssertErrorAsync(response, HttpStatusCode.BadRequest, code);
        using var scope = factory.Services.CreateScope();
        Assert.AreEqual(0, await scope.ServiceProvider.GetRequiredService<RegistryDbContext>().RegistrationTokens.CountAsync());
    }

    [TestMethod]
    [DataRow("1")]
    [DataRow("60")]
    [DataRow("86400")]
    public async Task IntegerTokenTtlBoundariesAreAccepted(string ttl)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var response = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + factory.AdminKey, "{\"ttlSeconds\":" + ttl + "}");
        Assert.AreEqual(HttpStatusCode.Created, response.StatusCode);
    }

    [TestMethod]
    public async Task BootstrapRefusesRedisplayAndRecoversOldestIdentityAfterLastRevocation()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var scope = factory.Services.CreateScope();
        var service = scope.ServiceProvider.GetRequiredService<CredentialService>();
        var conflict = await Assert.ThrowsExactlyAsync<DomainException>(() =>
            service.BootstrapAsync("ignored", CancellationToken.None));
        Assert.AreEqual("already_bootstrapped", conflict.Code);
        await service.RevokeAsync("admin", factory.AdminId, factory.AdminKey[5..29], CancellationToken.None);
        var recovered = await service.BootstrapAsync("ignored", CancellationToken.None);
        Assert.AreEqual(factory.AdminId, recovered.AdminId);
        Assert.AreNotEqual(factory.AdminKey, recovered.ApiKey);
        using var serialized = JsonDocument.Parse(JsonSerializer.Serialize(recovered, JsonSerializerOptions.Web));
        Assert.AreEqual(4, serialized.RootElement.EnumerateObject().Count());
        Assert.AreEqual(recovered.ApiKey, serialized.RootElement.GetProperty("key").GetString());
        Assert.IsFalse(serialized.RootElement.TryGetProperty("apiKey", out _));
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        Assert.AreEqual("administrator", (await db.Admins.AsNoTracking().SingleAsync()).Name);
        await service.RevokeAsync("admin", recovered.AdminId, recovered.KeyId, CancellationToken.None);
        var outcomes = await Task.WhenAll(this.TryBootstrapAsync(factory), this.TryBootstrapAsync(factory));
        Assert.AreEqual(1, outcomes.Count(outcome => outcome));
        Assert.AreEqual(1, outcomes.Count(outcome => !outcome));
        Assert.AreEqual(1, await db.ApiKeys.AsNoTracking().CountAsync(key => key.OwnerType == "admin" && key.RevokedAt == null));
    }

    [TestMethod]
    public async Task ResourceAuthorizationUsesUuidNotNamesOrAdministratorBypass()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("owner");
        var key = await factory.IssueMachineKeyAsync(id);
        using var scope = factory.Services.CreateScope();
        var credentials = scope.ServiceProvider.GetRequiredService<CredentialService>();
        var authenticated = await credentials.AuthenticateAsync(key, CancellationToken.None);
        Assert.AreEqual(id, authenticated!.OwnerId);
        var principal = new System.Security.Claims.ClaimsPrincipal(new System.Security.Claims.ClaimsIdentity(
        [
            new System.Security.Claims.Claim(System.Security.Claims.ClaimTypes.NameIdentifier, id),
            new System.Security.Claims.Claim(System.Security.Claims.ClaimTypes.Role, "machine")
        ], "ApiKey"));
        var authorization = scope.ServiceProvider.GetRequiredService<IAuthorizationService>();
        var requirement = new MachineOwnerRequirement();
        Assert.IsTrue((await authorization.AuthorizeAsync(principal, id, requirement)).Succeeded);
        Assert.IsFalse((await authorization.AuthorizeAsync(principal, "owner", requirement)).Succeeded);
        Assert.IsFalse((await authorization.AuthorizeAsync(principal, Guid.NewGuid().ToString(), requirement)).Succeeded);
        var admin = new System.Security.Claims.ClaimsPrincipal(new System.Security.Claims.ClaimsIdentity(
        [
            new System.Security.Claims.Claim(System.Security.Claims.ClaimTypes.NameIdentifier, factory.AdminId),
            new System.Security.Claims.Claim(System.Security.Claims.ClaimTypes.Role, "admin")
        ], "ApiKey"));
        Assert.IsFalse((await authorization.AuthorizeAsync(admin, id, requirement)).Succeeded);
    }

    [TestMethod]
    public async Task BootstrapKeyPersistenceFailureRollsBackNewIdentity()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var scope = factory.Services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        await db.ApiKeys.ExecuteDeleteAsync();
        await db.Admins.ExecuteDeleteAsync();
        await db.Database.ExecuteSqlRawAsync(
            """CREATE TRIGGER fail_bootstrap BEFORE INSERT ON api_keys BEGIN SELECT RAISE(ABORT, 'private-key-error'); END;""");
        var service = scope.ServiceProvider.GetRequiredService<CredentialService>();
        await Assert.ThrowsExactlyAsync<DbUpdateException>(() => service.BootstrapAsync("new administrator", CancellationToken.None));
        Assert.AreEqual(0, await db.Admins.AsNoTracking().CountAsync());
        Assert.AreEqual(0, await db.ApiKeys.AsNoTracking().CountAsync());
        await db.Database.ExecuteSqlRawAsync("DROP TRIGGER fail_bootstrap");
        using var retryScope = factory.Services.CreateScope();
        var retry = retryScope.ServiceProvider.GetRequiredService<CredentialService>();
        var result = await retry.BootstrapAsync("new administrator", CancellationToken.None);
        Assert.IsFalse(string.IsNullOrEmpty(result.ApiKey));
        Assert.AreEqual(result.AdminId, (await retry.AuthenticateAsync(result.ApiKey, CancellationToken.None))!.OwnerId);
    }

    [TestMethod]
    public async Task RegistrationConcurrentReusableTokenReturnsOneConflictWinnerAndOwnedKeys()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var created = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + factory.AdminKey, "{}");
        using var tokenBody = JsonDocument.Parse(await created.Content.ReadAsStringAsync());
        var token = tokenBody.RootElement.GetProperty("token").GetString()!;
        var names = new[] { "Café-host", "CAFE\u0301-HOST", "unique-one", "unique-two" };
        var registrations = await Task.WhenAll(names.Select(name =>
            SendAsync(factory, HttpMethod.Post, "/v1/agent/register", "Bearer " + token,
                JsonSerializer.Serialize(new { name }))));
        try
        {
            Assert.AreEqual(3, registrations.Count(response => response.StatusCode == HttpStatusCode.Created));
            Assert.AreEqual(1, registrations.Count(response => response.StatusCode == HttpStatusCode.Conflict));
            using var scope = factory.Services.CreateScope();
            var credentials = scope.ServiceProvider.GetRequiredService<CredentialService>();
            foreach (var response in registrations.Where(response => response.StatusCode == HttpStatusCode.Created))
            {
                using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
                var principal = await credentials.AuthenticateAsync(
                    body.RootElement.GetProperty("apiKey").GetString()!, CancellationToken.None);
                Assert.AreEqual(body.RootElement.GetProperty("id").GetString(), principal!.OwnerId);
                Assert.AreEqual("machine", principal.Role);
            }

            var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
            Assert.AreEqual(3, await db.Machines.AsNoTracking().CountAsync());
            Assert.AreEqual(4, await db.ApiKeys.AsNoTracking().CountAsync());
            Assert.AreEqual(1, await db.RegistrationTokens.AsNoTracking().CountAsync());
        }
        finally
        {
            foreach (var response in registrations)
            {
                response.Dispose();
            }
        }
    }

    [TestMethod]
    [DataRow(false)]
    [DataRow(true)]
    public async Task RegistrationKeyInsertOrCommitFailureRollsBackMachine(bool commitFailure)
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        using var created = await SendAsync(factory, HttpMethod.Post, "/v1/registration-tokens",
            "ApiKey " + factory.AdminKey, "{}");
        using var tokenBody = JsonDocument.Parse(await created.Content.ReadAsStringAsync());
        var token = tokenBody.RootElement.GetProperty("token").GetString()!;
        using var connection = new SqliteConnection($"Data Source={factory.DatabasePath};Foreign Keys=True");
        await connection.OpenAsync();
        using var command = connection.CreateCommand();
        command.CommandText = commitFailure
            ? """
                CREATE TABLE credential_fault_parent(id INTEGER PRIMARY KEY);
                CREATE TABLE credential_fault_child(id INTEGER REFERENCES credential_fault_parent(id) DEFERRABLE INITIALLY DEFERRED);
                CREATE TRIGGER fail_registration AFTER INSERT ON api_keys BEGIN INSERT INTO credential_fault_child VALUES (1); END;
                """
            : """CREATE TRIGGER fail_registration BEFORE INSERT ON api_keys BEGIN SELECT RAISE(ABORT, 'private-injected-error'); END;""";
        await command.ExecuteNonQueryAsync();
        using var failed = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register",
            "Bearer " + token, """{"name":"retry-host"}""");
        await AssertErrorAsync(failed, HttpStatusCode.ServiceUnavailable, "database_unavailable");
        var json = await failed.Content.ReadAsStringAsync();
        Assert.IsFalse(json.Contains("private-injected-error", StringComparison.Ordinal));
        Assert.IsFalse(json.Contains("apiKey", StringComparison.Ordinal));
        using var scope = factory.Services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        Assert.AreEqual(0, await db.Machines.CountAsync());
        Assert.AreEqual(1, await db.ApiKeys.CountAsync());
        command.CommandText = "DROP TRIGGER fail_registration";
        await command.ExecuteNonQueryAsync();
        using var retry = await SendAsync(factory, HttpMethod.Post, "/v1/agent/register",
            "Bearer " + token, """{"name":"retry-host"}""");
        Assert.AreEqual(HttpStatusCode.Created, retry.StatusCode);
    }

    internal static async Task<HttpResponseMessage> SendAsync(RegistryFactory factory, HttpMethod method,
        string path, string authorization, string? json = null)
    {
        using var client = factory.CreateClient();
        using var request = new HttpRequestMessage(method, path);
        if (authorization.Length != 0)
        {
            request.Headers.TryAddWithoutValidation("Authorization", authorization);
        }

        if (json is not null)
        {
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        return await client.SendAsync(request);
    }

    private static async Task AssertErrorAsync(HttpResponseMessage response, HttpStatusCode status, string code)
    {
        Assert.AreEqual(status, response.StatusCode);
        Assert.AreEqual("no-store", response.Headers.CacheControl!.ToString());
        using var json = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        Assert.AreEqual(code, json.RootElement.GetProperty("error").GetProperty("code").GetString());
        Assert.IsFalse(string.IsNullOrEmpty(json.RootElement.GetProperty("error").GetProperty("message").GetString()));
    }

    private async Task<bool> TryBootstrapAsync(RegistryFactory factory)
    {
        using var scope = factory.Services.CreateScope();
        var service = scope.ServiceProvider.GetRequiredService<CredentialService>();
        try
        {
            await service.BootstrapAsync("concurrent", CancellationToken.None);
            return true;
        }
        catch (DomainException exception) when (exception.Code == "already_bootstrapped")
        {
            return false;
        }
    }
}