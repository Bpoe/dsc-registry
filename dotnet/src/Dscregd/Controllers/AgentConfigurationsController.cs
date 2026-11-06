namespace Dscregd.Controllers;

using System.Security.Claims;
using Dscregd.Http;
using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.Options;
using Microsoft.Net.Http.Headers;

[ApiController]
[Authorize(Policy = "Machine")]
[Route("v1/agent/configurations")]
public class AgentConfigurationsController : ControllerBase
{
    private readonly ConfigurationService configurations;
    private readonly RegistryOptions options;

    public AgentConfigurationsController(ConfigurationService configurations, IOptions<RegistryOptions> options)
    {
        this.configurations = configurations;
        this.options = options.Value;
    }

    [HttpGet]
    [HttpHead]
    public async Task<IActionResult> Get()
    {
        var id = this.User.FindFirstValue(ClaimTypes.NameIdentifier)!;
        if (this.Request.Headers.TryGetValue("If-None-Match", out var validators))
        {
            var revision = await this.configurations.RevisionAsync(id, this.HttpContext.RequestAborted);
            var etag = Tag(id, revision);
            if (MatchesTag(string.Join(",", validators.ToArray()), etag))
            {
                this.Cache(etag);
                return this.StatusCode(304);
            }
        }

        var snapshot = await this.configurations.SnapshotAsync(id, this.HttpContext.RequestAborted);
        this.Cache(Tag(id, snapshot.ConfigurationRevision));
        return this.Ok(snapshot);
    }

    public static bool MatchesTag(string value, string etag)
    {
        if (!EntityTagHeaderValue.TryParseStrictList([value], out var validators))
        {
            return false;
        }

        return validators.Count == 1 && validators[0].Equals(EntityTagHeaderValue.Any) ||
            validators.All(validator => !validator.Equals(EntityTagHeaderValue.Any)) &&
            validators.Any(validator => validator.Compare(new EntityTagHeaderValue(etag), useStrongComparison: false));
    }

    private static string Tag(string id, long revision)
    {
        return $"\"machine-{id}-config-{revision}\"";
    }

    private void Cache(string etag)
    {
        this.Response.Headers.ETag = etag;
        this.Response.Headers.CacheControl = $"private, max-age={this.options.PollMaxAgeSeconds}";
        this.Response.Headers.Vary = "Authorization";
    }
}