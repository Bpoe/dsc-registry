namespace Dscregd.Authentication;

using System.Security.Claims;
using System.Text.Encodings.Web;
using System.Threading.Tasks;
using Dscregd.Services;
using Microsoft.AspNetCore.Authentication;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public class RegistrationAuthenticationHandler : RegistryAuthenticationHandler
{
    private readonly CredentialService credentials;

    public RegistrationAuthenticationHandler(IOptionsMonitor<AuthenticationSchemeOptions> options,
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

        if (!await this.credentials.AuthenticateRegistrationAsync(value, this.Context.RequestAborted))
        {
            return AuthenticateResult.Fail("Invalid registration token");
        }

        var identity = new ClaimsIdentity(
            [new Claim(ClaimTypes.Role, CredentialFormat.RegistrationRole)], this.Scheme.Name);
        return AuthenticateResult.Success(new AuthenticationTicket(new ClaimsPrincipal(identity), this.Scheme.Name));
    }
}