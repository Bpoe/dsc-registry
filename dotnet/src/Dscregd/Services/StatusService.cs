namespace Dscregd.Services;

using System;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Data;
using Dscregd.Reports;
using Microsoft.EntityFrameworkCore;

public class StatusService
{
    private readonly RegistryDbContext db;
    private readonly ReportService reports;

    public StatusService(RegistryDbContext db, ReportService reports)
    {
        this.db = db;
        this.reports = reports;
    }

    public async Task<ConfigurationStatus> GetConfigurationAsync(string machineId, string name, CancellationToken ct)
    {
        var metadata = await this.reports.GetConfigurationMetadataAsync(machineId, name, ct);
        return ConfigurationStatus.FromMetadata(metadata);
    }

    public async Task<MachineStatus> GetMachineAsync(string machineId, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        var machine = await this.db.LockMachineAsync(machineId, ct);
        var desired = await this.db.Configurations.AsNoTracking().Where(configuration => configuration.MachineId == machineId)
            .Select(configuration => new { configuration.Name, configuration.Revision }).ToArrayAsync(ct);
        var retained = await ReportService.LatestReports(this.db, machineId).ToArrayAsync(ct);
        await tx.CommitAsync(ct);

        var desiredByName = desired.ToDictionary(configuration => configuration.Name, configuration => configuration.Revision,
            StringComparer.Ordinal);
        var retainedByName = retained.ToDictionary(report => report.Name, StringComparer.Ordinal);
        var configurations = desiredByName.Keys.Union(retainedByName.Keys, StringComparer.Ordinal)
            .Order(StringComparer.Ordinal).Select(name =>
            {
                desiredByName.TryGetValue(name, out var revision);
                retainedByName.TryGetValue(name, out var report);
                var received = report == null ? null : ReportMetadata.FromReport(report);
                var current = revision != null && report?.ConfigurationRevision == revision ? received : null;
                var revisionState = revision == null ? "removed" : current != null ? "current" :
                    report == null ? "unreported" : "stale";
                var outcome = current?.Outcome ?? (revisionState == "unreported" ? "never_reported" : "unknown");
                return new MachineConfigurationStatus(name, revision, revisionState, outcome, current, received);
            }).ToArray();
        var latestReceived = retained.OrderByDescending(report => report.Sequence).FirstOrDefault();
        var latestCurrent = retained.Where(report => desiredByName.TryGetValue(report.Name, out var revision) &&
            revision == report.ConfigurationRevision).OrderByDescending(report => report.Sequence).FirstOrDefault();
        var currentConfigurations = configurations.Where(configuration => configuration.DesiredRevision != null).ToArray();
        var aggregate = retained.Length == 0 ? "never_reported" :
            currentConfigurations.Length == 0 ? "unknown" :
            currentConfigurations.Any(configuration => configuration.Outcome == "failed") ? "failed" :
            currentConfigurations.Any(configuration => configuration.Outcome == "canceled") ? "canceled" :
            currentConfigurations.Any(configuration => configuration.LatestReport == null) ? "unknown" : "succeeded";
        return new MachineStatus(machineId, machine.ConfigurationRevision, retained.Length > 0,
            latestReceived?.ReceivedAt, latestCurrent?.FinishedAt,
            latestCurrent == null ? null : ReportMetadata.FromReport(latestCurrent),
            latestReceived == null ? null : ReportMetadata.FromReport(latestReceived),
            aggregate, configurations.Any(configuration => configuration.RevisionState == "stale"), configurations);
    }
}