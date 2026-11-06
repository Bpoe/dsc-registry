namespace Dscregd.Authentication;

using System;
using System.Net.Http.Headers;
using System.Text.Encodings.Web;
using System.Threading.Tasks;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public abstract class RegistryAuthenticationHandler : AuthenticationHandler<AuthenticationSchemeOptions>
{
    protected RegistryAuthenticationHandler(IOptionsMonitor<AuthenticationSchemeOptions> options,
        ILoggerFactory logger, UrlEncoder encoder)
        : base(options, logger, encoder)
    {
    }

    protected bool TryGetCredential(out string value)
    {
        value = "";
        var headers = this.Request.Headers.Authorization;
        if (headers.Count != 1 || !AuthenticationHeaderValue.TryParse(headers[0], out var header) ||
            !string.Equals(header.Scheme, this.Scheme.Name, StringComparison.OrdinalIgnoreCase) ||
            header.Parameter is null)
        {
            return false;
        }

        value = header.Parameter;
        return true;
    }

    protected override Task HandleChallengeAsync(AuthenticationProperties properties)
    {
        this.Response.StatusCode = StatusCodes.Status401Unauthorized;
        this.Response.Headers.WWWAuthenticate = this.Scheme.Name;
        this.Response.Headers.CacheControl = "no-store";
        var message = this.Scheme.Name == RegistryAuthentication.RegistrationScheme
            ? "Valid registration token required"
            : "Valid API key required";
        return this.Response.WriteAsJsonAsync(new
        {
            error = new
            {
                code = "unauthorized",
                message
            }
        },
            options: null, contentType: "application/json", cancellationToken: this.Context.RequestAborted);
    }

    protected override Task HandleForbiddenAsync(AuthenticationProperties properties)
    {
        this.Response.StatusCode = StatusCodes.Status403Forbidden;
        this.Response.Headers.CacheControl = "no-store";
        return this.Response.WriteAsJsonAsync(
            new
            {
                error = new
                {
                    code = "forbidden",
                    message = "Credential role is not authorized for this operation"
                }
            },
            options: null, contentType: "application/json", cancellationToken: this.Context.RequestAborted);
    }
}