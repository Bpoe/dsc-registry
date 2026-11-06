namespace Dscregd.Controllers;

using System.Threading.Tasks;
using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;

[ApiController]
public class RegistrationController : ControllerBase
{
    private readonly CredentialService credentials;
    private readonly MachineService machines;

    public RegistrationController(CredentialService credentials, MachineService machines)
    {
        this.credentials = credentials;
        this.machines = machines;
    }

    [Authorize(Policy = "Admin")]
    [HttpPost("v1/registration-tokens")]
    public async Task<IActionResult> CreateTokenAsync([FromBody] RegistrationTokenRequest request)
    {
        return this.StatusCode(201, await this.credentials.CreateRegistrationTokenAsync(request.TtlSeconds, this.HttpContext.RequestAborted));
    }

    [Authorize(Policy = "Registration")]
    [HttpPost("v1/agent/register")]
    public async Task<IActionResult> RegisterAsync([FromBody] MachineNameRequest request)
    {
        var machine = await this.machines.RegisterAsync(request.Name, this.HttpContext.RequestAborted);
        return this.Created("/v1/machines/" + machine.Id, machine);
    }
}