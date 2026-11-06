namespace Dscregd.Data;

using Microsoft.EntityFrameworkCore;
using Microsoft.EntityFrameworkCore.Infrastructure;
using Microsoft.EntityFrameworkCore.Migrations;

[DbContext(typeof(RegistryDbContext))]
[Migration("20261106000000_GoCompatibleBaseline")]
public class GoCompatibleBaseline : Migration
{
    protected override void Up(MigrationBuilder migrationBuilder)
    {
        var engine = this.ActiveProvider == "Npgsql.EntityFrameworkCore.PostgreSQL" ? "postgres" : "sqlite";
        var assembly = typeof(RegistryDbContext).Assembly;
        var files = assembly.GetManifestResourceNames()
            .Where(x => x.Contains($".Schema.{engine}.", StringComparison.Ordinal)).Order().ToArray();
        if (files.Length != (engine == "sqlite" ? 4 : 5))
        {
            throw new InvalidOperationException("Embedded compatibility schema is incomplete.");
        }

        migrationBuilder.Sql("CREATE TABLE schema_migrations (version INTEGER NOT NULL PRIMARY KEY, applied_at TEXT NOT NULL)");
        for (var i = 0; i < files.Length; i++)
        {
            using var stream = assembly.GetManifestResourceStream(files[i])!;
            using var reader = new StreamReader(stream);
            // EF creates the baseline using the original DDL, including provider-specific
            // triggers/check constraints which cannot be expressed by entity mappings.
            migrationBuilder.Sql(reader.ReadToEnd());
            migrationBuilder.Sql($"INSERT INTO schema_migrations VALUES ({i + 1}, '{Timestamp.Now()}')");
        }
    }

    protected override void Down(MigrationBuilder migrationBuilder)
        => throw new NotSupportedException("Downgrading the shared Go-compatible schema is not supported.");
}