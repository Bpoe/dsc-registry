namespace Dscregd.Controllers;

using System.Security.Claims;
using System.Text.Json;
using System.Threading.Tasks;
using Dscregd.Http;
using Dscregd.Reports;
using Dscregd.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.Options;

[ApiController]
public class ReportsController : ControllerBase
{
    private readonly JsonSerializerOptions json;
    private readonly ReportService reports;
    private readonly StatusService status;

    public ReportsController(JsonSerializerOptions json, ReportService reports, StatusService status)
    {
        this.json = json;
        this.reports = reports;
        this.status = status;
    }

    [HttpPost("/v1/agent/reports")]
    [Authorize(Policy = "Machine")]
    public async Task<IActionResult> Upload([FromBody] ReportSubmission submission)
    {
        var upload = ReportUpload.FromSubmission(submission, this.json);
        var machineId = this.User.FindFirstValue(ClaimTypes.NameIdentifier)!;
        var receipt = await this.reports.UploadAsync(machineId, upload, this.HttpContext.RequestAborted);
        this.Response.Headers.Location = "/v1/machines/" + machineId + "/reports/" + receipt.Metadata.Id;
        return this.StatusCode(receipt.Created ? 201 : 200, receipt.Metadata);
    }

    [HttpGet("/v1/machines/{id}/reports")]
    [HttpHead("/v1/machines/{id}/reports")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> List(Guid id, [FromQuery] PaginationRequest pagination)
    {
        return this.Ok(await this.reports.ListAsync(id.ToString(), pagination.Limit, pagination.Cursor,
            this.HttpContext.RequestAborted));
    }

    [HttpGet("/v1/machines/{id}/reports/{reportId}")]
    [HttpHead("/v1/machines/{id}/reports/{reportId}")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> Metadata(Guid id, Guid reportId)
    {
        return this.Ok(await this.reports.GetMetadataAsync(id.ToString(), reportId.ToString(), this.HttpContext.RequestAborted));
    }

    [HttpGet("/v1/machines/{id}/reports/{reportId}/content")]
    [HttpHead("/v1/machines/{id}/reports/{reportId}/content")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> ReadContent(Guid id, Guid reportId)
    {
        return this.File(await this.reports.GetContentAsync(id.ToString(), reportId.ToString(), this.HttpContext.RequestAborted), "application/json");
    }

    [HttpGet("/v1/machines/{id}/status")]
    [HttpHead("/v1/machines/{id}/status")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> MachineStatus(Guid id)
    {
        return this.Ok(await this.status.GetMachineAsync(id.ToString(), this.HttpContext.RequestAborted));
    }

    [HttpGet("/v1/machines/{id}/configurations/{name}/status")]
    [HttpHead("/v1/machines/{id}/configurations/{name}/status")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> ConfigurationStatus([FromRoute] ConfigurationRoute route)
    {
        return this.Ok(await this.status.GetConfigurationAsync(route.Id.ToString(), route.Name, this.HttpContext.RequestAborted));
    }

    [HttpGet("/v1/machines/{id}/configurations/{name}/report")]
    [HttpHead("/v1/machines/{id}/configurations/{name}/report")]
    [Authorize(Policy = "Admin")]
    public async Task<IActionResult> ConfigurationReport([FromRoute] ConfigurationRoute route)
    {
        return this.File(await this.reports.GetConfigurationContentAsync(route.Id.ToString(), route.Name, this.HttpContext.RequestAborted), "application/json");
    }
}