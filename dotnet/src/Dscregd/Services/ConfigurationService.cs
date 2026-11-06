namespace Dscregd.Services;

using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Dscregd.Data;
using Dscregd.Eventing;
using Dscregd.Reports;
using Microsoft.EntityFrameworkCore;

public class ConfigurationService
{
    private readonly RegistryDbContext db;
    private readonly EventDispatcher events;
    private readonly ReportStorage storage;
    private readonly JsonSerializerOptions json;

    public ConfigurationService(RegistryDbContext db, EventDispatcher events, ReportStorage storage, JsonSerializerOptions json)
    {
        this.db = db;
        this.events = events;
        this.storage = storage;
        this.json = json;
    }

    public async Task<(ConfigurationResult Result, bool Created)> PutAsync(string id, string name,
        JsonElement template, JsonElement? parameters, CancellationToken ct)
    {
        if (!IsDocument(template))
        {
            throw InvalidContent();
        }

        try
        {
            // Reject values the native comparer cannot handle before publishing them.
            _ = JsonElement.DeepEquals(template, template);
            if (parameters is { ValueKind: not JsonValueKind.Null } parameter)
            {
                if (!IsDocument(parameter))
                {
                    throw InvalidContent();
                }

                _ = JsonElement.DeepEquals(parameter, parameter);
            }
        }
        catch (Exception error) when (error is ArgumentOutOfRangeException or InvalidOperationException or JsonException)
        {
            throw InvalidContent();
        }

        var templateJson = JsonSerializer.Serialize(template, this.json);
        string? parametersJson = null;
        if (parameters is { ValueKind: not JsonValueKind.Null } parameterValue)
        {
            if (!IsDocument(parameterValue))
            {
                throw InvalidContent();
            }

            parametersJson = JsonSerializer.Serialize(parameterValue, this.json);
        }

        await using var tx = await this.db.BeginMutationAsync(ct);
        var machine = await this.db.LockMachineAsync(id, ct);
        var current = await this.db.Configurations.SingleOrDefaultAsync(x => x.MachineId == id && x.Name == name, ct);
        var created = current is null;
        bool changed;
        try
        {
            changed = created ||
                !JsonElement.DeepEquals(JsonSerializer.Deserialize<JsonElement>(current!.TemplateJson, this.json), template) ||
                (current.ParametersJson is null) != (parametersJson is null) ||
                current.ParametersJson is not null && parametersJson is not null &&
                !JsonElement.DeepEquals(JsonSerializer.Deserialize<JsonElement>(current.ParametersJson, this.json), parameters!.Value);
        }
        catch (ArgumentOutOfRangeException)
        {
            throw InvalidContent();
        }
        if (changed)
        {
            if (current is null)
            {
                current = new DesiredConfiguration { MachineId = id, Name = name };
                this.db.Configurations.Add(current);
            }

            current.Revision = Guid.NewGuid().ToString();
            current.TemplateJson = templateJson;
            current.ParametersJson = parametersJson;
            current.ContentSha256 = SHA256.HashData(Encoding.UTF8.GetBytes(templateJson + "\0" + parametersJson));
            current.UpdatedAt = Timestamp.Now();
            machine.ConfigurationRevision = checked(machine.ConfigurationRevision + 1);
            await this.db.SaveChangesAsync(ct);
        }

        var result = this.Result(current!, machine.ConfigurationRevision);
        await tx.CommitAsync(ct);
        if (changed)
        {
            this.events.Notify(created ? "configuration.created" : "configuration.updated", id, name);
            this.events.Notify("machine.updated", id);
        }

        return (result, created);
    }

    public async Task<ConfigurationResult> GetAsync(string id, string name, CancellationToken ct)
    {
        var rows = await (
            from machine in this.db.Machines.AsNoTracking()
            where machine.Id == id
            join config in this.db.Configurations.AsNoTracking().Where(x => x.Name == name)
                on machine.Id equals config.MachineId into configs
            from config in configs.DefaultIfEmpty()
            select new { machine.ConfigurationRevision, Config = config }).ToListAsync(ct);
        if (rows.Count == 0)
        {
            throw DomainException.MachineNotFound();
        }

        var row = rows[0];
        return row.Config is null
            ? throw DomainException.ConfigurationNotFound()
            : this.Result(row.Config, row.ConfigurationRevision);
    }

    public async Task<ConfigurationPage> ListAsync(string id, int limit, string cursor, CancellationToken ct)
    {
        if (!await this.db.Machines.AsNoTracking().AnyAsync(x => x.Id == id, ct))
        {
            throw DomainException.MachineNotFound();
        }

        var rows = await this.db.Configurations.AsNoTracking()
            .Where(x => x.MachineId == id && string.Compare(x.Name, cursor) > 0)
            .OrderBy(x => x.Name).Take(limit + 1)
            .Select(x => new ConfigurationSummary(x.Name, x.Revision)).ToListAsync(ct);
        return new(rows.Take(limit).ToArray(), rows.Count > limit ? rows[limit - 1].Name : null);
    }

    public async Task DeleteAsync(string id, string name, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        var machine = await this.db.LockMachineAsync(id, ct);
        var current = await this.db.Configurations.SingleOrDefaultAsync(x => x.MachineId == id && x.Name == name, ct)
            ?? throw DomainException.ConfigurationNotFound();
        var paths = await this.db.Reports.Where(x => x.MachineId == id && x.Name == name)
            .Select(x => x.ReportPath).ToListAsync(ct);
        this.db.Configurations.Remove(current);
        await this.db.Reports.Where(x => x.MachineId == id && x.Name == name).ExecuteDeleteAsync(ct);
        machine.ConfigurationRevision = checked(machine.ConfigurationRevision + 1);
        await this.db.SaveChangesAsync(ct);
        await tx.CommitAsync(ct);
        this.events.Notify("configuration.deleted", id, name);
        this.events.Notify("machine.updated", id);
        this.storage.RemoveFiles(paths);
    }

    public async Task<long> RevisionAsync(string id, CancellationToken ct)
    {
        return await this.db.Machines.AsNoTracking().Where(x => x.Id == id)
            .Select(x => (long?)x.ConfigurationRevision).SingleOrDefaultAsync(ct)
            ?? throw DomainException.MachineNotFound();
    }

    public async Task<ConfigurationSnapshot> SnapshotAsync(string id, CancellationToken ct)
    {
        // A single LINQ left join becomes one ordered SQL snapshot, including an empty collection.
        var rows = await (
            from machine in this.db.Machines.AsNoTracking()
            where machine.Id == id
            join config in this.db.Configurations.AsNoTracking() on machine.Id equals config.MachineId into configs
            from config in configs.DefaultIfEmpty()
            orderby config.Name
            select new { machine.ConfigurationRevision, Config = config }).ToListAsync(ct);
        if (rows.Count == 0)
        {
            throw DomainException.MachineNotFound();
        }

        return new(rows[0].ConfigurationRevision,
            rows.Where(x => x.Config is not null).Select(x => this.Content(x.Config!)).ToArray());
    }

    private static bool IsDocument(JsonElement value)
    {
        return value.ValueKind == JsonValueKind.Object &&
            !(value.TryGetProperty("format", out _) && value.TryGetProperty("content", out _));
    }

    private static DomainException InvalidContent()
    {
        return new(400, "invalid_configuration", "Expected a JSON-object template and optional JSON-object parameters");
    }

    private ConfigurationContent Content(DesiredConfiguration value)
    {
        return new(value.Name, value.Revision, JsonSerializer.Deserialize<JsonElement>(value.TemplateJson, this.json),
            value.ParametersJson is null ? null : JsonSerializer.Deserialize<JsonElement>(value.ParametersJson, this.json));
    }

    private ConfigurationResult Result(DesiredConfiguration value, long revision)
    {
        return new(value.Name, value.Revision, revision, JsonSerializer.Deserialize<JsonElement>(value.TemplateJson, this.json),
            value.ParametersJson is null ? null : JsonSerializer.Deserialize<JsonElement>(value.ParametersJson, this.json));
    }
}