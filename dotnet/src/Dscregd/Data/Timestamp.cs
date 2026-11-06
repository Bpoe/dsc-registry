namespace Dscregd.Data;

public static class Timestamp
{
    public static string Now() => DateTime.UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'",
        System.Globalization.CultureInfo.InvariantCulture);
}