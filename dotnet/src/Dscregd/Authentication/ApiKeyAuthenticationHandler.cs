namespace Dscregd.Authentication;

using System.Security.Claims;
using System.Text.Encodings.Web;
using System.Threading.Tasks;
using Dscregd.Services;
using Microsoft.AspNetCore.Authentication;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public class ApiKeyAuthenticationHandler : RegistryAuthenticationHandler
{
    private readonly CredentialService credentials;

    public ApiKeyAuthenticationHandler(IOptionsMonitor<AuthenticationSchemeOptions> options,
        ILoggerFactory logger, UrlEncoder encoder, CredentialService credentials)
        : base(options, logger, encoder)
    {
        this.credentials = credentials;
    }

    protected override async Task<AuthenticateResult> HandleAuthenticateAsync()
    {
        if (!this.TryGetCredential(out var value))
        {
            return AuthenticateResult.NoResult();
        }

        var principal = await this.credentials.AuthenticateAsync(value, this.Context.RequestAborted);
        if (principal == null)
        {
            return AuthenticateResult.Fail("Invalid API key");
        }

        var identity = new ClaimsIdentity(
        [
            new Claim(ClaimTypes.NameIdentifier, principal.OwnerId),
            new Claim(ClaimTypes.Role, principal.Role),
            new Claim("key_id", principal.KeyId)
        ], this.Scheme.Name);
        return AuthenticateResult.Success(new AuthenticationTicket(new ClaimsPrincipal(identity), this.Scheme.Name));
    }
}