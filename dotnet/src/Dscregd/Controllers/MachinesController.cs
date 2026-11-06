namespace Dscregd.Controllers;

using System;
using System.Threading.Tasks;
using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;

[ApiController]
[Authorize(Policy = "Admin")]
[Route("v1/machines")]
public class MachinesController : ControllerBase
{
    private readonly MachineService machines;

    public MachinesController(MachineService machines)
    {
        this.machines = machines;
    }

    [HttpPost]
    public async Task<IActionResult> CreateAsync([FromBody] MachineNameRequest request)
    {
        var machine = await this.machines.CreateAsync(request.Name, this.HttpContext.RequestAborted);
        return this.Created("/v1/machines/" + machine.Id, machine);
    }

    [HttpGet]
    [HttpHead]
    public async Task<IActionResult> ListAsync([FromQuery] string? name, [FromQuery] PaginationRequest pagination)
    {
        if (this.Request.Query.ContainsKey("name"))
        {
            return this.Ok(await this.machines.FindByNameAsync(name ?? "", this.HttpContext.RequestAborted));
        }

        return this.Ok(await this.machines.ListAsync(pagination.Limit, pagination.Cursor, this.HttpContext.RequestAborted));
    }

    [HttpGet("{id}")]
    [HttpHead("{id}")]
    public async Task<IActionResult> GetAsync(Guid id)
    {
        return this.Ok(await this.machines.GetAsync(id.ToString(), this.HttpContext.RequestAborted));
    }

    [HttpDelete("{id}")]
    public async Task<IActionResult> DeleteAsync(Guid id)
    {
        await this.machines.DeleteAsync(id.ToString(), this.HttpContext.RequestAborted);
        this.Response.ContentType = "application/json";
        return this.NoContent();
    }

}