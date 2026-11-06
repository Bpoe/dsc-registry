namespace Dscregd.Controllers;

using System.ComponentModel.DataAnnotations;
using System.Threading.Tasks;
using Dscregd.Authentication;
using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;

[ApiController]
[Authorize(Policy = "Admin")]
public class CredentialsController : ControllerBase
{
    private readonly CredentialService credentials;

    public CredentialsController(CredentialService credentials)
    {
        this.credentials = credentials;
    }

    [HttpPost("v1/machines/{id}/api-keys")]
    public Task<IActionResult> IssueMachineAsync(Guid id, [FromBody] KeyRequest request)
    {
        return this.IssueAsync(CredentialFormat.MachineRole, id, request);
    }

    [HttpPost("v1/admins/{id}/api-keys")]
    public Task<IActionResult> IssueAdminAsync(Guid id, [FromBody] KeyRequest request)
    {
        return this.IssueAsync(CredentialFormat.AdminRole, id, request);
    }

    [HttpGet("v1/machines/{id}/api-keys")]
    [HttpHead("v1/machines/{id}/api-keys")]
    public Task<IActionResult> ListMachineAsync(Guid id, [FromQuery] PaginationRequest pagination)
    {
        return this.ListAsync(CredentialFormat.MachineRole, id, pagination);
    }

    [HttpGet("v1/admins/{id}/api-keys")]
    [HttpHead("v1/admins/{id}/api-keys")]
    public Task<IActionResult> ListAdminAsync(Guid id, [FromQuery] PaginationRequest pagination)
    {
        return this.ListAsync(CredentialFormat.AdminRole, id, pagination);
    }

    [HttpDelete("v1/machines/{id}/api-keys/{keyId}")]
    public Task<IActionResult> RevokeMachineAsync(Guid id, [RegularExpression(CredentialFormat.KeyIdPattern)] string keyId)
    {
        return this.RevokeAsync(CredentialFormat.MachineRole, id, keyId);
    }

    [HttpDelete("v1/admins/{id}/api-keys/{keyId}")]
    public Task<IActionResult> RevokeAdminAsync(Guid id, [RegularExpression(CredentialFormat.KeyIdPattern)] string keyId)
    {
        return this.RevokeAsync(CredentialFormat.AdminRole, id, keyId);
    }

    private async Task<IActionResult> IssueAsync(string role, Guid id, KeyRequest request)
    {
        return this.StatusCode(201, await this.credentials.IssueAsync(role, id.ToString(), request.Description, this.HttpContext.RequestAborted));
    }

    private async Task<IActionResult> ListAsync(string role, Guid id, PaginationRequest pagination)
    {
        return this.Ok(await this.credentials.ListAsync(role, id.ToString(), pagination.Limit, pagination.Cursor, this.HttpContext.RequestAborted));
    }

    private async Task<IActionResult> RevokeAsync(string role, Guid id, string keyId)
    {
        await this.credentials.RevokeAsync(role, id.ToString(), keyId, this.HttpContext.RequestAborted);
        this.Response.ContentType = "application/json";
        return this.NoContent();
    }
}