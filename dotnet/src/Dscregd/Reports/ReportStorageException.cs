namespace Dscregd.Reports;

using System;

internal class ReportStorageException : Exception
{
    public ReportStorageException(bool isMissing, Exception innerException)
        : base("Report storage operation unavailable", innerException)
    {
        this.IsMissing = isMissing;
    }

    public bool IsMissing { get; }
}