namespace Dscregd.Data;

using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;

public static class DatabaseInitialization
{
    public static async Task InitializeAsync(IServiceProvider services, CancellationToken ct)
    {
        using var scope = services.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<RegistryDbContext>();
        var options = scope.ServiceProvider.GetRequiredService<IOptions<RegistryOptions>>().Value;
        if (db.Database.IsSqlite())
        {
            var builder = new SqliteConnectionStringBuilder(options.Database.ConnectionString);
            var path = builder.DataSource;
            if (path is "" or ":memory:" || path.StartsWith("file:", StringComparison.OrdinalIgnoreCase) ||
                path.StartsWith(@"\\", StringComparison.Ordinal) || builder.Mode == SqliteOpenMode.Memory)
            {
                throw new InvalidOperationException("SQLite requires a persistent local database file.");
            }

            var absolute = Path.GetFullPath(path);
            Directory.CreateDirectory(Path.GetDirectoryName(absolute)!);
            if (!File.Exists(absolute))
            {
                var fileOptions = new FileStreamOptions { Mode = FileMode.CreateNew, Access = FileAccess.ReadWrite };
                if (!OperatingSystem.IsWindows())
                {
                    fileOptions.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
                }

                using var file = new FileStream(absolute, fileOptions);
            }
            await db.Database.OpenConnectionAsync(ct);
            await db.Database.ExecuteSqlRawAsync("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;", ct);
        }
        else
        {
            await db.Database.OpenConnectionAsync(ct);
        }

        // Inspect only the schema ledger; never run a new EF baseline over a Go database.
        var connection = db.Database.GetDbConnection();
        await using var command = connection.CreateCommand();
        command.CommandText = db.Database.IsSqlite()
            ? "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'"
            : "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='schema_migrations'";
        var hasLedger = Convert.ToInt64(await command.ExecuteScalarAsync(ct)) != 0;
        if (hasLedger)
        {
            command.CommandText = "SELECT COUNT(*), COALESCE(MAX(version),0) FROM schema_migrations";
            await using var reader = await command.ExecuteReaderAsync(ct);
            await reader.ReadAsync(ct);
            var expected = db.Database.IsSqlite() ? 4 : 5;
            if (Convert.ToInt32(reader.GetValue(0)) != expected || Convert.ToInt32(reader.GetValue(1)) != expected)
            {
                throw new InvalidOperationException("Unsupported Go schema. Upgrade with the Go server before using this experiment.");
            }
        }
        else
        {
            command.CommandText = db.Database.IsSqlite()
                ? "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
                : "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema()";
            if (Convert.ToInt64(await command.ExecuteScalarAsync(ct)) != 0)
            {
                throw new InvalidOperationException("Refusing to initialize a nonempty database without a recognized schema ledger.");
            }

            await db.Database.MigrateAsync(ct);
        }
        // Validate mapped columns even for empty tables.
        await db.Machines.Take(1).AsNoTracking().ToListAsync(ct);
        await db.Admins.Take(1).AsNoTracking().ToListAsync(ct);
        await db.ApiKeys.Take(1).AsNoTracking().ToListAsync(ct);
        await db.Configurations.Take(1).AsNoTracking().ToListAsync(ct);
        await db.Reports.Take(1).AsNoTracking().ToListAsync(ct);
        await db.RegistrationTokens.Take(1).AsNoTracking().ToListAsync(ct);
    }
}