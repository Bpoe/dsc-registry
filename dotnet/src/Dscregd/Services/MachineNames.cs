namespace Dscregd.Services;

using System.Globalization;
using System.Text;

public static class MachineNames
{
    private static readonly UTF8Encoding utf8 = new(false, true);

    public static string CreateLookupKey(string value)
    {
        try
        {
            return value.Normalize(NormalizationForm.FormC)
                .ToLowerInvariant()
                .Normalize(NormalizationForm.FormC);
        }
        catch (ArgumentException)
        {
            throw new DomainException(400, "invalid_name", "Name cannot be normalized as Unicode text");
        }
    }

    public static void ValidateDisplayName(string value, int maxBytes, string code)
    {
        int byteCount;
        try
        {
            byteCount = utf8.GetByteCount(value);
        }
        catch (EncoderFallbackException)
        {
            throw new DomainException(400, code, "Expected valid Unicode text");
        }

        if (value.Length == 0 || byteCount > maxBytes || value != value.Trim())
        {
            throw new DomainException(400, code, "Value exceeds its UTF-8 byte limit or has surrounding whitespace or control characters");
        }

        foreach (var rune in value.EnumerateRunes())
        {
            if (Rune.GetUnicodeCategory(rune) == UnicodeCategory.Control)
            {
                throw new DomainException(400, code, "Value exceeds its UTF-8 byte limit or has surrounding whitespace or control characters");
            }
        }
    }
}