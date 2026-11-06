namespace Dscregd.Services;

using System;
using System.Linq;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Data;
using Dscregd.Eventing;
using Dscregd.Reports;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Logging;

public class ReportService
{
    private readonly RegistryDbContext db;
    private readonly ReportStorage storage;
    private readonly EventDispatcher events;
    private readonly ILogger<ReportService> logger;

    public ReportService(RegistryDbContext db, ReportStorage storage, EventDispatcher events,
        ILogger<ReportService> logger)
    {
        this.db = db;
        this.storage = storage;
        this.events = events;
        this.logger = logger;
    }

    public async Task<ReportReceipt> UploadAsync(string machineId, ReportUpload upload, CancellationToken ct)
    {
        var digest = SHA256.HashData(upload.Result);
        try
        {
            while (true)
            {
                ct.ThrowIfCancellationRequested();
                var retained = await LatestReports(this.db, machineId)
                    .SingleOrDefaultAsync(report => report.Name == upload.Name && report.UploadId == upload.UploadId, ct);
                string? verifiedId = null;
                string? candidate = null;
                if (retained != null)
                {
                    if (retained.ConfigurationRevision != upload.ConfigurationRevision)
                    {
                        throw UploadConflict();
                    }

                    var storedDigest = retained.ResultSha256;
                    if (storedDigest == null)
                    {
                        try
                        {
                            var content = await this.storage.ReadAsync(retained.ReportPath, ct);
                            storedDigest = SHA256.HashData(content);
                        }

                        catch (ReportStorageException error) when (error.IsMissing)
                        {
                            var currentId = await LatestReports(this.db, machineId)
                                .Where(report => report.Name == upload.Name)
                                .Select(report => report.Id).SingleOrDefaultAsync(ct);
                            if (currentId != retained.Id)
                            {
                                continue;
                            }

                            throw;
                        }
                    }

                    if (!CryptographicOperations.FixedTimeEquals(storedDigest, digest))
                    {
                        throw UploadConflict();
                    }

                    verifiedId = retained.Id;
                }

                else
                {
                    candidate = await this.storage.WriteAsync(upload.Result, ct);
                }

                var keepCandidate = false;
                Publication publication;
                try
                {
                    publication = await this.PublishAsync(machineId, upload, digest, candidate,
                        verifiedId, () => keepCandidate = true, ct);
                }

                finally
                {
                    // PublishAsync has disposed its transaction before any cleanup.
                    // Once COMMIT is attempted, a candidate may be referenced even
                    // when the acknowledgement fails; retain it conservatively.
                    if (candidate != null && !keepCandidate)
                    {
                        this.storage.RemoveFiles([candidate]);
                    }
                }

                if (publication.Restart)
                {
                    continue;
                }

                if (publication.Changed)
                {
                    this.events.Notify(publication.Replaced ? "report.updated" : "report.created",
                        machineId, upload.Name, publication.Metadata!.Id);
                }

                this.storage.RemoveFiles(publication.RetiredPaths);
                return new ReportReceipt(publication.Metadata!, publication.Changed);
            }
        }

        catch (ReportStorageException)
        {
            throw this.StorageUnavailable();
        }
    }

    public async Task<ReportPage> ListAsync(string machineId, int limit, string cursor, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        await this.db.LockMachineAsync(machineId, ct);
        var query = LatestReports(this.db, machineId);
        if (cursor.Length != 0)
        {
            var sequence = await query.Where(report => report.Id == cursor)
                .Select(report => (long?)report.Sequence).SingleOrDefaultAsync(ct);
            if (sequence == null)
            {
                throw new DomainException(400, "invalid_pagination", "Report cursor does not belong to this machine");
            }

            query = query.Where(report => report.Sequence < sequence.Value);
        }

        var reports = await query.OrderByDescending(report => report.Sequence).Take(limit + 1).ToArrayAsync(ct);
        await tx.CommitAsync(ct);
        var nextCursor = reports.Length > limit ? reports[limit - 1].Id : null;
        return new ReportPage(reports.Take(limit).Select(ReportMetadata.FromReport).ToArray(), nextCursor);
    }

    public async Task<ReportMetadata> GetMetadataAsync(string machineId, string reportId, CancellationToken ct)
    {
        var report = await this.SelectReportAsync(machineId, reportId, ct);
        return ReportMetadata.FromReport(report);
    }

    public async Task<byte[]> GetContentAsync(string machineId, string reportId, CancellationToken ct)
    {
        var report = await this.SelectReportAsync(machineId, reportId, ct);
        try
        {
            return await this.storage.ReadAsync(report.ReportPath, ct);
        }

        catch (ReportStorageException error)
        {
            if (error.IsMissing)
            {
                // A concurrent replacement/deletion may have retired the file.
                // An obsolete ID is 404; a still-published missing file is 503.
                await this.SelectReportAsync(machineId, reportId, ct);
            }

            throw this.StorageUnavailable();
        }
    }

    public async Task<ReportMetadata> GetConfigurationMetadataAsync(string machineId, string name, CancellationToken ct)
    {
        var report = await this.SelectConfigurationReportAsync(machineId, name, ct);
        return ReportMetadata.FromReport(report);
    }

    public async Task<byte[]> GetConfigurationContentAsync(string machineId, string name, CancellationToken ct)
    {
        string? missingId = null;
        while (true)
        {
            ct.ThrowIfCancellationRequested();
            var report = await this.SelectConfigurationReportAsync(machineId, name, ct);
            if (report.Id == missingId)
            {
                throw this.StorageUnavailable();
            }

            try
            {
                return await this.storage.ReadAsync(report.ReportPath, ct);
            }

            catch (ReportStorageException error) when (error.IsMissing)
            {
                missingId = report.Id;
            }

            catch (ReportStorageException)
            {
                throw this.StorageUnavailable();
            }
        }
    }

    internal static IQueryable<Report> LatestReports(RegistryDbContext db, string machineId)
    {
        return db.Reports.AsNoTracking().Where(report => report.MachineId == machineId &&
            report.Sequence == db.Reports.Where(other => other.MachineId == report.MachineId && other.Name == report.Name)
                .Max(other => other.Sequence));
    }

    private static DomainException UploadConflict()
    {
        return new DomainException(409, "upload_id_conflict", "Upload ID already identifies a different report");
    }

    private async Task<Publication> PublishAsync(string machineId, ReportUpload upload, byte[] digest,
        string? candidate, string? verifiedId, Action preserveCandidate, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        await this.db.LockMachineAsync(machineId, ct);
        var retained = await LatestReports(this.db, machineId)
            .SingleOrDefaultAsync(report => report.Name == upload.Name && report.UploadId == upload.UploadId, ct);
        if (retained != null)
        {
            if (retained.ConfigurationRevision != upload.ConfigurationRevision)
            {
                throw UploadConflict();
            }

            if (retained.ResultSha256 == null)
            {
                if (retained.Id != verifiedId)
                {
                    return Publication.Retry();
                }

                // The file's digest was verified outside the transaction, then
                // the immutable retained ID was rechecked under the machine lock.
                await this.db.Reports.Where(report => report.Id == retained.Id && report.ResultSha256 == null)
                    .ExecuteUpdateAsync(setters => setters.SetProperty(report => report.ResultSha256, digest), ct);
            }

            else if (!CryptographicOperations.FixedTimeEquals(retained.ResultSha256, digest))
            {
                throw UploadConflict();
            }

            await tx.CommitAsync(ct);
            return new Publication(ReportMetadata.FromReport(retained), false, false, false, []);
        }

        if (candidate == null)
        {
            return Publication.Retry();
        }

        var maximumSequence = await this.db.Reports.Where(report => report.MachineId == machineId)
            .Select(report => (long?)report.Sequence).MaxAsync(ct) ?? 0;
        var previous = await this.db.Reports.Where(report => report.MachineId == machineId && report.Name == upload.Name)
            .Select(report => report.ReportPath).Distinct().ToArrayAsync(ct);
        await this.db.Reports.Where(report => report.MachineId == machineId && report.Name == upload.Name)
            .ExecuteDeleteAsync(ct);
        var accepted = new Report
        {
            Id = Guid.NewGuid().ToString(),
            MachineId = machineId,
            Sequence = checked(maximumSequence + 1),
            Name = upload.Name,
            ConfigurationRevision = upload.ConfigurationRevision,
            ReceivedAt = Timestamp.Now(),
            StartedAt = upload.StartedAt,
            FinishedAt = upload.FinishedAt,
            Outcome = upload.Outcome,
            Operation = upload.Operation,
            InputHash = upload.InputHash,
            ErrorKind = upload.ErrorKind,
            ReportPath = candidate,
            UploadId = upload.UploadId,
            ResultSha256 = digest
        };
        this.db.Reports.Add(accepted);
        await this.db.SaveChangesAsync(ct);
        preserveCandidate();
        await tx.CommitAsync(ct);
        return new Publication(ReportMetadata.FromReport(accepted), true, previous.Length > 0, false, previous);
    }

    private async Task<Report> SelectReportAsync(string machineId, string reportId, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        await this.db.LockMachineAsync(machineId, ct);
        var report = await LatestReports(this.db, machineId)
            .SingleOrDefaultAsync(item => item.Id == reportId, ct) ?? throw DomainException.ReportNotFound();
        await tx.CommitAsync(ct);
        return report;
    }

    private async Task<Report> SelectConfigurationReportAsync(string machineId, string name, CancellationToken ct)
    {
        await using var tx = await this.db.BeginMutationAsync(ct);
        await this.db.LockMachineAsync(machineId, ct);
        var revision = await this.db.Configurations.AsNoTracking()
            .Where(configuration => configuration.MachineId == machineId && configuration.Name == name)
            .Select(configuration => configuration.Revision).SingleOrDefaultAsync(ct) ??
            throw DomainException.ConfigurationNotFound();
        var report = await LatestReports(this.db, machineId)
            .SingleOrDefaultAsync(item => item.Name == name && item.ConfigurationRevision == revision, ct) ??
            throw DomainException.ReportNotFound();
        await tx.CommitAsync(ct);
        return report;
    }

    private DomainException StorageUnavailable()
    {
        this.logger.LogError("Report storage operation failed");
        return new DomainException(503, "report_storage_unavailable", "Report storage operation unavailable");
    }

    private record Publication(
        ReportMetadata? Metadata,
        bool Changed,
        bool Replaced,
        bool Restart,
        string[] RetiredPaths)
    {
        public static Publication Retry()
        {
            return new Publication(null, false, false, true, []);
        }
    }
}