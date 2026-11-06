namespace Dscregd.Http;

using Microsoft.AspNetCore.Http.Features;
using Microsoft.AspNetCore.Mvc.Controllers;
using Microsoft.AspNetCore.Mvc.Filters;
using Microsoft.Extensions.Options;

public class RequestBodyLimitFilter : IResourceFilter
{
    private readonly RegistryOptions options;

    public RequestBodyLimitFilter(IOptions<RegistryOptions> options)
    {
        this.options = options.Value;
    }

    public void OnResourceExecuting(ResourceExecutingContext context)
    {
        var feature = context.HttpContext.Features.Get<IHttpMaxRequestBodySizeFeature>();
        if (feature is null || feature.IsReadOnly)
        {
            throw new InvalidOperationException("The server must support request-size limits before model binding.");
        }

        var action = (ControllerActionDescriptor)context.ActionDescriptor;
        feature.MaxRequestBodySize = action.ControllerName == "Reports" && action.ActionName == "Upload"
            ? this.options.MaxReportBytes : this.options.MaxRequestBytes;
    }

    public void OnResourceExecuted(ResourceExecutedContext context)
    {
    }
}