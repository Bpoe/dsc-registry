namespace Dscregd.Data;

using Microsoft.EntityFrameworkCore;
using Microsoft.EntityFrameworkCore.Storage;

public class RegistryDbContext(DbContextOptions<RegistryDbContext> options) : DbContext(options)
{
    public DbSet<Machine> Machines => this.Set<Machine>();
    public DbSet<Admin> Admins => this.Set<Admin>();
    public DbSet<ApiKey> ApiKeys => this.Set<ApiKey>();
    public DbSet<RegistrationToken> RegistrationTokens => this.Set<RegistrationToken>();
    public DbSet<DesiredConfiguration> Configurations => this.Set<DesiredConfiguration>();
    public DbSet<Report> Reports => this.Set<Report>();

    public async Task<IDbContextTransaction> BeginMutationAsync(CancellationToken ct)
        => await this.Database.BeginTransactionAsync(ct);

    public async Task<Machine> LockMachineAsync(string id, CancellationToken ct)
    {
        Machine? machine;
        if (this.Database.IsNpgsql())
        {
            // LINQ has no row-lock operator. All machine mutations use this same lock.
            machine = await this.Machines.FromSqlInterpolated($"SELECT * FROM machines WHERE id = {id} FOR UPDATE")
                .SingleOrDefaultAsync(ct);
        }
        else
        {
            // Microsoft.Data.Sqlite starts a non-deferred (IMMEDIATE) EF transaction.
            machine = await this.Machines.SingleOrDefaultAsync(x => x.Id == id, ct);
        }
        return machine ?? throw DomainException.MachineNotFound();
    }

    public async Task LockAdminAsync(string id, CancellationToken ct)
    {
        var exists = this.Database.IsNpgsql()
            ? await this.Admins.FromSqlInterpolated($"SELECT * FROM admins WHERE id = {id} FOR UPDATE")
                .AnyAsync(ct)
            : await this.Admins.AnyAsync(x => x.Id == id, ct);
        if (!exists)
        {
            throw new DomainException(404, "admin_not_found", "Administrator not found");
        }
    }

    public async Task<IDbContextTransaction> BeginBootstrapAsync(CancellationToken ct)
    {
        var tx = await this.BeginMutationAsync(ct);
        if (this.Database.IsNpgsql())
        {
            await this.Database.ExecuteSqlRawAsync("LOCK TABLE admins IN EXCLUSIVE MODE", ct);
        }

        return tx;
    }

    protected override void OnModelCreating(ModelBuilder model)
    {
        model.Entity<Machine>().ToTable("machines").HasKey(x => x.Id);
        model.Entity<Machine>().HasIndex(x => x.NameFolded).IsUnique()
            .HasDatabaseName("machines_name_folded_unique");
        model.Entity<Admin>().ToTable("admins").HasKey(x => x.Id);
        model.Entity<ApiKey>().ToTable("api_keys").HasKey(x => x.KeyId);
        model.Entity<ApiKey>().HasIndex(x => new { x.OwnerType, x.OwnerId })
            .HasDatabaseName("api_keys_owner");
        model.Entity<RegistrationToken>().ToTable("registration_tokens").HasKey(x => x.TokenHash);
        model.Entity<DesiredConfiguration>().ToTable("configurations")
            .HasKey(x => new { x.MachineId, x.Name });
        model.Entity<DesiredConfiguration>().HasOne<Machine>().WithMany()
            .HasForeignKey(x => x.MachineId).OnDelete(DeleteBehavior.Cascade);
        model.Entity<Report>().ToTable("reports").HasKey(x => x.Id);
        model.Entity<Report>().HasOne<Machine>().WithMany()
            .HasForeignKey(x => x.MachineId).OnDelete(DeleteBehavior.Cascade);
        model.Entity<Report>().HasIndex(x => new { x.MachineId, x.Sequence }).IsUnique();
        model.Entity<Report>().HasIndex(x => new { x.MachineId, x.Name, x.UploadId }).IsUnique();
        model.Entity<Report>().HasIndex(x => new { x.MachineId, x.Name, x.Sequence })
            .HasDatabaseName("reports_configuration_latest");
        model.Entity<Report>().HasIndex(x => new { x.MachineId, x.Name, x.ConfigurationRevision, x.Sequence })
            .HasDatabaseName("reports_configuration_revision_latest");
        model.Entity<Report>().HasIndex(x => x.ReportPath).HasDatabaseName("reports_path");
        // Go stores UUIDs, exact JSON and RFC3339 timestamps as text on both engines.
        foreach (var entity in model.Model.GetEntityTypes())
        {
            foreach (var property in entity.GetProperties())
            {
                var column = System.Text.RegularExpressions.Regex.Replace(property.Name, "([a-z0-9])([A-Z])", "$1_$2")
                    .ToLowerInvariant();
                property.SetColumnName(column);
            }
        }
    }
}