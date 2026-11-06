namespace Dscregd.Services;

using System;
using System.Globalization;
using System.Linq;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;
using Dscregd.Authentication;
using Dscregd.Data;
using Microsoft.EntityFrameworkCore;

public class CredentialService
{
    private readonly RegistryDbContext db;

    public CredentialService(RegistryDbContext db)
    {
        this.db = db;
    }

    public async Task<BootstrapResult> BootstrapAsync(string name, CancellationToken ct)
    {
        MachineNames.ValidateDisplayName(name, 256, "invalid_name");
        await using var transaction = await this.db.BeginBootstrapAsync(ct);
        if (await this.db.ApiKeys.AsNoTracking()
            .AnyAsync(key => key.OwnerType == CredentialFormat.AdminRole && key.RevokedAt == null, ct))
        {
            throw new DomainException(409, "already_bootstrapped", "A usable administrator key already exists");
        }

        var admin = await this.db.Admins.AsNoTracking().OrderBy(value => value.CreatedAt).ThenBy(value => value.Id)
            .FirstOrDefaultAsync(ct);
        if (admin == null)
        {
            admin = new Admin
            {
                Name = name
            };
            this.db.Admins.Add(admin);
            await this.db.SaveChangesAsync(ct);
        }

        var issued = await this.InsertKeyAsync(CredentialFormat.AdminRole, admin.Id, "local bootstrap", ct);
        await transaction.CommitAsync(ct);
        return new BootstrapResult(admin.Id, issued.KeyId, issued.Key, issued.CreatedAt);
    }

    public async Task<AuthenticatedCredential?> AuthenticateAsync(string value, CancellationToken ct)
    {
        if (!CredentialFormat.TryParse(value, out var role, out var keyId))
        {
            return null;
        }

        var key = await this.db.ApiKeys.AsNoTracking()
            .SingleOrDefaultAsync(key => key.KeyId == keyId && key.RevokedAt == null, ct);
        if (key == null || !CryptographicOperations.FixedTimeEquals(CredentialFormat.Digest(value), key.SecretHash) ||
            role != key.OwnerType)
        {
            return null;
        }

        return new AuthenticatedCredential(role, key.OwnerId, key.KeyId);
    }

    public async Task<bool> AuthenticateRegistrationAsync(string value, CancellationToken ct)
    {
        var digest = CredentialFormat.Digest(value);
        var token = await this.db.RegistrationTokens.AsNoTracking()
            .SingleOrDefaultAsync(token => token.TokenHash == digest, ct);
        if (token == null)
        {
            return false;
        }

        if (!DateTimeOffset.TryParse(token.ExpiresAt, CultureInfo.InvariantCulture,
            DateTimeStyles.AssumeUniversal, out var expiration))
        {
            throw new DomainException(503, "database_unavailable", "Database operation unavailable");
        }

        return DateTimeOffset.UtcNow < expiration;
    }

    public async Task<IssuedKey> IssueAsync(string role, string ownerId, string description, CancellationToken ct)
    {
        if (description != "")
        {
            MachineNames.ValidateDisplayName(description, 1024, "invalid_description");
        }

        await using var transaction = await this.db.BeginMutationAsync(ct);
        await this.LockOwnerAsync(role, ownerId, ct);
        var key = await this.InsertKeyAsync(role, ownerId, description, ct);
        await transaction.CommitAsync(ct);
        return key;
    }

    public async Task<KeyPage> ListAsync(string role, string ownerId, int limit, string cursor, CancellationToken ct)
    {
        await using var transaction = await this.db.BeginMutationAsync(ct);
        await this.LockOwnerAsync(role, ownerId, ct);
        var keys = await this.db.ApiKeys.AsNoTracking()
            .Where(key => key.OwnerType == role && key.OwnerId == ownerId && string.Compare(key.KeyId, cursor) > 0)
            .OrderBy(key => key.KeyId).Take(limit + 1)
            .Select(key => new KeyMetadata(key.KeyId, key.OwnerType, key.OwnerId, key.CreatedAt,
                key.RevokedAt, key.Description ?? "")).ToListAsync(ct);
        var nextCursor = keys.Count > limit ? keys[limit - 1].KeyId : null;
        if (keys.Count > limit)
        {
            keys.RemoveAt(limit);
        }

        await transaction.CommitAsync(ct);
        return new KeyPage(keys, nextCursor);
    }

    public async Task RevokeAsync(string role, string ownerId, string keyId, CancellationToken ct)
    {
        await using var transaction = await this.db.BeginMutationAsync(ct);
        await this.LockOwnerAsync(role, ownerId, ct);
        var now = Timestamp.Now();
        var affected = await this.db.ApiKeys
            .Where(key => key.OwnerType == role && key.OwnerId == ownerId && key.KeyId == keyId)
            .ExecuteUpdateAsync(setters => setters.SetProperty(key => key.RevokedAt, key => key.RevokedAt ?? now), ct);
        if (affected == 0)
        {
            throw new DomainException(404, role + "_not_found", "Owner or key not found");
        }

        await transaction.CommitAsync(ct);
    }

    public async Task<IssuedRegistrationToken> CreateRegistrationTokenAsync(int ttlSeconds, CancellationToken ct)
    {
        if (ttlSeconds is < 1 or > 86400)
        {
            throw new DomainException(400, "invalid_ttl", "ttlSeconds must be an integer between 1 and 86400");
        }

        var token = CredentialFormat.GenerateRegistrationToken();
        var expiration = DateTime.UtcNow.AddSeconds(ttlSeconds)
            .ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", CultureInfo.InvariantCulture);
        await using var transaction = await this.db.BeginMutationAsync(ct);
        this.db.RegistrationTokens.Add(new RegistrationToken
        {
            TokenHash = CredentialFormat.Digest(token),
            ExpiresAt = expiration
        });
        await this.db.SaveChangesAsync(ct);
        await transaction.CommitAsync(ct);
        return new IssuedRegistrationToken(token, expiration);
    }

    public static bool ValidKeyId(string value)
    {
        return CredentialFormat.ValidKeyId(value);
    }

    internal async Task<IssuedKey> InsertKeyAsync(string role, string ownerId, string description, CancellationToken ct)
    {
        var generated = CredentialFormat.Generate(role);
        var createdAt = Timestamp.Now();
        this.db.ApiKeys.Add(new ApiKey
        {
            KeyId = generated.KeyId,
            OwnerType = role,
            OwnerId = ownerId,
            SecretHash = CredentialFormat.Digest(generated.Key),
            CreatedAt = createdAt,
            Description = description
        });
        await this.db.SaveChangesAsync(ct);
        return new IssuedKey(generated.KeyId, generated.Key, createdAt);
    }

    private async Task LockOwnerAsync(string role, string ownerId, CancellationToken ct)
    {
        if (role == CredentialFormat.AdminRole)
        {
            await this.db.LockAdminAsync(ownerId, ct);
        }
        else if (role == CredentialFormat.MachineRole)
        {
            await this.db.LockMachineAsync(ownerId, ct);
        }
        else
        {
            throw new ArgumentException("Unknown credential role", nameof(role));
        }
    }
}