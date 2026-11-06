namespace Dscregd.Reports;

using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public class ReportStorage
{
    private readonly RegistryOptions options;
    private readonly ILogger<ReportStorage> logger;
    private string? root;

    public ReportStorage(IOptions<RegistryOptions> options, ILogger<ReportStorage> logger)
    {
        this.options = options.Value;
        this.logger = logger;
    }

    public async Task InitializeAsync(CancellationToken ct)
    {
        ct.ThrowIfCancellationRequested();
        try
        {
            if (!Directory.Exists(this.options.ReportRoot))
            {
                if (OperatingSystem.IsWindows())
                {
                    Directory.CreateDirectory(this.options.ReportRoot);
                }

                else
                {
                    Directory.CreateDirectory(this.options.ReportRoot,
                        UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
                }
            }

            var canonical = CanonicalDirectory(this.options.ReportRoot);
            this.root = canonical;
            var probe = await this.WriteAsync("0"u8.ToArray(), ct);
            try
            {
                var content = await this.ReadAsync(probe, ct);
                if (!content.AsSpan().SequenceEqual("0"u8))
                {
                    throw new IOException("Report probe failed");
                }
            }

            finally
            {
                this.RemoveFiles([probe]);
            }
        }

        catch
        {
            this.root = null;
            throw;
        }
    }

    public async Task<string> WriteAsync(ReadOnlyMemory<byte> content, CancellationToken ct)
    {
        ct.ThrowIfCancellationRequested();
        var name = Guid.NewGuid().ToString() + ".json";
        var created = false;
        try
        {
            var path = this.ResolvePath(name);
            var fileOptions = new FileStreamOptions
            {
                Mode = FileMode.CreateNew,
                Access = FileAccess.Write,
                Share = FileShare.None,
                Options = FileOptions.Asynchronous,
                BufferSize = 8192
            };
            if (!OperatingSystem.IsWindows())
            {
                fileOptions.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
            }

            await using (var file = new FileStream(path, fileOptions))
            {
                created = true;
                await file.WriteAsync(content, ct);
                await file.FlushAsync(ct);
                file.Flush(flushToDisk: true);
            }

            ct.ThrowIfCancellationRequested();
            return name;
        }

        catch (Exception error) when (error is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            if (created)
            {
                this.RemoveFiles([name]);
            }

            throw new ReportStorageException(false, error);
        }

        catch
        {
            if (created)
            {
                this.RemoveFiles([name]);
            }

            throw;
        }
    }

    public async Task<byte[]> ReadAsync(string relativePath, CancellationToken ct)
    {
        ct.ThrowIfCancellationRequested();
        try
        {
            var path = this.ResolvePath(relativePath);
            await using var file = new FileStream(path, FileMode.Open, FileAccess.Read,
                FileShare.Read | FileShare.Delete, 8192, FileOptions.Asynchronous | FileOptions.SequentialScan);
            if (file.Length > this.options.MaxReportBytes)
            {
                throw new IOException("Stored report exceeds limit");
            }

            using var content = new MemoryStream();
            var chunk = new byte[8192];
            int count;
            while ((count = await file.ReadAsync(chunk, ct)) != 0)
            {
                if (content.Length + count > this.options.MaxReportBytes)
                {
                    throw new IOException("Stored report exceeds limit");
                }

                await content.WriteAsync(chunk.AsMemory(0, count), ct);
            }

            return content.ToArray();
        }

        catch (Exception error) when (error is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            throw new ReportStorageException(error is FileNotFoundException or DirectoryNotFoundException, error);
        }
    }

    public void RemoveFiles(IEnumerable<string> paths)
    {
        foreach (var relativePath in paths.Distinct(StringComparer.Ordinal))
        {
            try
            {
                var path = this.ResolvePath(relativePath, removing: true);
                File.Delete(path);
            }

            catch (Exception error) when (error is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
            {
                if (error is not (FileNotFoundException or DirectoryNotFoundException))
                {
                    this.logger.LogWarning(error, "Report file cleanup failed for {Path}", relativePath);
                }
            }
        }
    }

    private static string CanonicalDirectory(string configured)
    {
        var absolute = Path.GetFullPath(configured);
        var volume = Path.GetPathRoot(absolute)!;
        var current = volume;
        foreach (var component in absolute[volume.Length..].Split(Path.DirectorySeparatorChar,
            StringSplitOptions.RemoveEmptyEntries))
        {
            var info = new DirectoryInfo(Path.Combine(current, component));
            if (!info.Exists)
            {
                throw new DirectoryNotFoundException("Report root must be an existing directory");
            }

            current = info.ResolveLinkTarget(returnFinalTarget: true)?.FullName ?? info.FullName;
        }

        return Path.GetFullPath(current);
    }

    private static void RejectLink(string path, bool allowMissing)
    {
        try
        {
            var attributes = File.GetAttributes(path);
            if ((attributes & FileAttributes.ReparsePoint) != 0)
            {
                throw new IOException("Symbolic links within report storage are not permitted");
            }
        }

        catch (FileNotFoundException) when (allowMissing)
        {
        }

        catch (DirectoryNotFoundException) when (allowMissing)
        {
        }
    }

    private string ResolvePath(string relativePath, bool removing = false)
    {
        if (this.root == null)
        {
            throw new IOException("Report storage has not initialized");
        }

        if (string.IsNullOrEmpty(relativePath) || Path.IsPathRooted(relativePath) ||
            relativePath.Contains(':') || relativePath.IndexOf('\0') >= 0)
        {
            throw new IOException("Invalid report storage key");
        }

        var components = relativePath.Split(['/', '\\']);
        if (components.Any(component => component.Length == 0 || component is "." or ".." ||
            component.EndsWith('.') || component.EndsWith(' ') ||
            component.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0))
        {
            throw new IOException("Invalid report storage key");
        }

        // Resolve trusted root aliases once at startup; never follow the
        // configured alias again. Standard .NET APIs lack os.Root's handle-
        // relative confinement, so operator-controlled parents remain required.
        var current = Path.GetPathRoot(this.root)!;
        foreach (var component in this.root[current.Length..].Split(Path.DirectorySeparatorChar,
            StringSplitOptions.RemoveEmptyEntries))
        {
            current = Path.Combine(current, component);
            RejectLink(current, allowMissing: false);
        }

        for (var i = 0; i < components.Length; i++)
        {
            current = Path.Combine(current, components[i]);
            if (i != components.Length - 1 || !removing)
            {
                RejectLink(current, allowMissing: i == components.Length - 1 || removing);
            }
        }

        var relative = Path.GetRelativePath(this.root, current);
        if (Path.IsPathRooted(relative) || relative == ".." ||
            relative.StartsWith(".." + Path.DirectorySeparatorChar, StringComparison.Ordinal))
        {
            throw new IOException("Report path escapes storage root");
        }

        return current;
    }
}