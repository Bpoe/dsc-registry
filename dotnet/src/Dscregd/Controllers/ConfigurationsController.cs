namespace Dscregd.Controllers;

using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;

[ApiController]
[Authorize(Policy = "Admin")]
[Route("v1/machines/{id}/configurations")]
public class ConfigurationsController : ControllerBase
{
    private readonly ConfigurationService configurations;

    public ConfigurationsController(ConfigurationService configurations)
    {
        this.configurations = configurations;
    }

    [HttpGet]
    [HttpHead]
    public async Task<IActionResult> List(Guid id, [FromQuery] PaginationRequest pagination)
    {
        return this.Ok(await this.configurations.ListAsync(id.ToString(), pagination.Limit, pagination.Cursor,
            this.HttpContext.RequestAborted));
    }

    [HttpPut("{name}")]
    public async Task<IActionResult> Put([FromRoute] ConfigurationRoute route, [FromBody] ConfigurationRequest request)
    {
        var (result, created) = await this.configurations.PutAsync(route.Id.ToString(), route.Name, request.Template,
            request.Parameters, this.HttpContext.RequestAborted);
        if (created)
        {
            return this.Created($"/v1/machines/{route.Id}/configurations/{route.Name}", result);
        }

        return this.Ok(result);
    }

    [HttpGet("{name}")]
    [HttpHead("{name}")]
    public async Task<IActionResult> Get([FromRoute] ConfigurationRoute route)
    {
        return this.Ok(await this.configurations.GetAsync(route.Id.ToString(), route.Name, this.HttpContext.RequestAborted));
    }

    [HttpDelete("{name}")]
    public async Task<IActionResult> Delete([FromRoute] ConfigurationRoute route)
    {
        await this.configurations.DeleteAsync(route.Id.ToString(), route.Name, this.HttpContext.RequestAborted);
        return this.NoContent();
    }
}