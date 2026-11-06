namespace Dscregd.Authentication;

using System;
using System.Security.Cryptography;
using System.Text;

public static class CredentialFormat
{
    public const string AdminRole = "admin";
    public const string MachineRole = "machine";
    public const string RegistrationRole = "registration";
    public const string KeyIdPattern = "^k_[A-Za-z0-9_-]{21}[AQgw]$";

    public static bool TryParse(string value, out string role, out string keyId)
    {
        role = "";
        keyId = "";
        if (value.Length != 73 || (value[..5] != "dsca_" && value[..5] != "dscm_") ||
            value[29] != '.' || !ValidKeyId(value[5..29]) || !ValidEncoding(value[30..], 32))
        {
            return false;
        }

        role = value[..5] == "dsca_" ? AdminRole : MachineRole;
        keyId = value[5..29];
        return true;
    }

    public static bool ValidKeyId(string value)
    {
        return value.StartsWith("k_", StringComparison.Ordinal) && ValidEncoding(value[2..], 16);
    }

    public static (string KeyId, string Key) Generate(string role)
    {
        var prefix = role switch
        {
            AdminRole => "dsca_",
            MachineRole => "dscm_",
            _ => throw new ArgumentException("Unknown credential role", nameof(role))
        };
        var keyId = "k_" + Encode(RandomNumberGenerator.GetBytes(16));
        return (keyId, prefix + keyId + "." + Encode(RandomNumberGenerator.GetBytes(32)));
    }

    public static string GenerateRegistrationToken()
    {
        return "dscr_" + Encode(RandomNumberGenerator.GetBytes(32));
    }

    public static byte[] Digest(string value)
    {
        return SHA256.HashData(Encoding.UTF8.GetBytes(value));
    }

    private static string Encode(byte[] bytes)
    {
        return Convert.ToBase64String(bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_');
    }

    private static bool ValidEncoding(string value, int size)
    {
        if (value.Length != (size * 8 + 5) / 6 ||
            value.AsSpan().ContainsAnyExcept("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"))
        {
            return false;
        }

        try
        {
            var decoded = Convert.FromBase64String(value.Replace('-', '+').Replace('_', '/') +
                new string('=', (4 - value.Length % 4) % 4));
            return decoded.Length == size && Encode(decoded) == value;
        }
        catch (FormatException)
        {
            return false;
        }
    }
}