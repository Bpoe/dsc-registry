namespace Dscregd.Services;

public record KeyMetadata(
    string KeyId,
    string OwnerType,
    string OwnerId,
    string CreatedAt,
    string? RevokedAt,
    string Description);