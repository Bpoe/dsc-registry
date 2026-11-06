namespace Dscregd.Tests;

using System.Net.Http.Json;
using Dscregd.Data;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class DatabaseTests
{
    [TestMethod]
    public async Task BaselineIsGoCompatibleAndReopeningDoesNotApplyOverExistingRows()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("persisted-host");
        using var scope = factory.Services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        await db.Database.OpenConnectionAsync();
        await using var query = db.Database.GetDbConnection().CreateCommand();
        query.CommandText = "SELECT COUNT(*), MAX(version) FROM schema_migrations";
        await using (var rows = await query.ExecuteReaderAsync())
        {
            await rows.ReadAsync();
            Assert.AreEqual(4L, rows.GetInt64(0));
            Assert.AreEqual(4L, rows.GetInt64(1));
        }

        query.CommandText = "PRAGMA foreign_keys";
        Assert.AreEqual(1L, Convert.ToInt64(await query.ExecuteScalarAsync()));
        query.CommandText = "PRAGMA synchronous";
        Assert.AreEqual(2L, Convert.ToInt64(await query.ExecuteScalarAsync()));
        await DatabaseInitialization.InitializeAsync(factory.Services, CancellationToken.None);
        var machine = await db.Machines.AsNoTracking().SingleAsync(x => x.Id == id);
        Assert.AreEqual("persisted-host", machine.Name);
        Assert.AreEqual(0L, machine.ConfigurationRevision);
    }

    [TestMethod]
    public async Task DesiredMutationFailureDoesNotBumpRevisionOrPublishPartialConfiguration()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("rollback-host");
        using var scope = factory.Services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        await db.Database.ExecuteSqlRawAsync("""
            CREATE TRIGGER fail_configuration BEFORE INSERT ON configurations
            BEGIN SELECT RAISE(ABORT, 'injected failure'); END;
            """);
        using var response = await factory.Client.PutAsJsonAsync($"/v1/machines/{id}/configurations/example",
            new { template = new { } });
        Assert.AreEqual(503, (int)response.StatusCode);
        Assert.AreEqual(0, (await db.Configurations.AsNoTracking().ToListAsync()).Count);
        Assert.AreEqual(0L, await db.Machines.Where(x => x.Id == id).Select(x => x.ConfigurationRevision).SingleAsync());
    }

    [TestMethod]
    public async Task ConcurrentWritesAndSnapshotsNeverMixCollectionRevisionWithDocuments()
    {
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        var id = await factory.CreateMachineAsync("snapshot-host");
        var writes = Enumerable.Range(0, 16).Select(async number =>
        {
            using var response = await factory.Client.PutAsJsonAsync(
                $"/v1/machines/{id}/configurations/item-{number:d2}", new { template = new { number } });
            response.EnsureSuccessStatusCode();
        });
        var reader = Task.Run(async () =>
        {
            for (var i = 0; i < 20; i++)
            {
                using var scope = factory.Services.CreateScope();
                var snapshot = await scope.ServiceProvider.GetRequiredService<Services.ConfigurationService>()
                    .SnapshotAsync(id, CancellationToken.None);
                Assert.AreEqual(snapshot.ConfigurationRevision, (long)snapshot.Configurations.Length);
            }
        });
        await Task.WhenAll(writes.Append(reader));
    }
}