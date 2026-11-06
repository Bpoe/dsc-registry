namespace Dscregd.Services;

using System.Collections.Generic;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Authentication;
using Dscregd.Data;
using Dscregd.Eventing;
using Dscregd.Reports;
using Microsoft.Data.Sqlite;
using Microsoft.EntityFrameworkCore;
using Npgsql;

public class MachineService
{
    private readonly RegistryDbContext db;
    private readonly EventDispatcher events;
    private readonly ReportStorage storage;
    private readonly CredentialService credentials;

    public MachineService(RegistryDbContext db, EventDispatcher events, ReportStorage storage, CredentialService credentials)
    {
        this.db = db;
        this.events = events;
        this.storage = storage;
        this.credentials = credentials;
    }

    public async Task<MachineDetails> CreateAsync(string name, CancellationToken ct)
    {
        MachineNames.ValidateDisplayName(name, 256, "invalid_name");
        await using var transaction = await this.db.BeginMutationAsync(ct);
        var machine = await this.InsertMachineAsync(name, ct);
        await transaction.CommitAsync(ct);
        this.events.Notify("machine.created", machine.Id);
        return Details(machine);
    }

    public async Task<RegisteredMachine> RegisterAsync(string name, CancellationToken ct)
    {
        MachineNames.ValidateDisplayName(name, 256, "invalid_name");
        await using var transaction = await this.db.BeginMutationAsync(ct);
        var machine = await this.InsertMachineAsync(name, ct);
        var key = await this.credentials.InsertKeyAsync(CredentialFormat.MachineRole, machine.Id, "", ct);
        await transaction.CommitAsync(ct);
        this.events.Notify("machine.created", machine.Id);
        return new RegisteredMachine(machine.Id, machine.Name, key.Key);
    }

    public async Task<MachineDetails> GetAsync(string id, CancellationToken ct)
    {
        var machine = await this.db.Machines.AsNoTracking().SingleOrDefaultAsync(machine => machine.Id == id, ct);
        return machine == null ? throw DomainException.MachineNotFound() : Details(machine);
    }

    public async Task<MachineDetails> FindByNameAsync(string name, CancellationToken ct)
    {
        MachineNames.ValidateDisplayName(name, 256, "invalid_name");
        var folded = MachineNames.CreateLookupKey(name);
        var machine = await this.db.Machines.AsNoTracking()
            .SingleOrDefaultAsync(machine => machine.NameFolded == folded, ct);
        return machine == null ? throw DomainException.MachineNotFound() : Details(machine);
    }

    public async Task<MachinePage> ListAsync(int limit, string cursor, CancellationToken ct)
    {
        var machines = await this.db.Machines.AsNoTracking()
            .Where(machine => string.Compare(machine.Id, cursor) > 0).OrderBy(machine => machine.Id).Take(limit + 1)
            .Select(machine => new MachineDetails(machine.Id, machine.Name, machine.ConfigurationRevision))
            .ToListAsync(ct);
        var nextCursor = machines.Count > limit ? machines[limit - 1].Id : null;
        if (machines.Count > limit)
        {
            machines.RemoveAt(limit);
        }

        return new MachinePage(machines, nextCursor);
    }

    public async Task DeleteAsync(string id, CancellationToken ct)
    {
        List<string> paths;
        await using (var transaction = await this.db.BeginMutationAsync(ct))
        {
            await this.db.LockMachineAsync(id, ct);
            paths = await this.db.Reports.AsNoTracking().Where(report => report.MachineId == id)
                .Select(report => report.ReportPath).ToListAsync(ct);
            await this.db.Machines.Where(machine => machine.Id == id).ExecuteDeleteAsync(ct);
            await transaction.CommitAsync(ct);
        }

        this.events.Notify("machine.deleted", id);
        this.storage.RemoveFiles(paths);
    }

    private static MachineDetails Details(Machine machine)
    {
        return new MachineDetails(machine.Id, machine.Name, machine.ConfigurationRevision);
    }

    private async Task<Machine> InsertMachineAsync(string name, CancellationToken ct)
    {
        var machine = new Machine
        {
            Name = name,
            NameFolded = MachineNames.CreateLookupKey(name)
        };
        this.db.Machines.Add(machine);
        try
        {
            await this.db.SaveChangesAsync(ct);
        }
        catch (DbUpdateException exception) when (
            exception.InnerException is PostgresException { SqlState: "23505", ConstraintName: "machines_name_folded_unique" } ||
            exception.InnerException is SqliteException { SqliteExtendedErrorCode: 2067 } sqlite &&
                sqlite.Message.Contains("machines.name_folded", System.StringComparison.Ordinal))
        {
            throw new DomainException(409, "machine_name_conflict", "A machine with this name already exists");
        }

        return machine;
    }
}