namespace Dscregd.Authentication;

using System;
using System.Security.Claims;
using System.Threading.Tasks;
using Microsoft.AspNetCore.Authorization;

public class MachineOwnerAuthorizationHandler : AuthorizationHandler<MachineOwnerRequirement, string>
{
    protected override Task HandleRequirementAsync(AuthorizationHandlerContext context,
        MachineOwnerRequirement requirement, string resource)
    {
        if (context.User.IsInRole(CredentialFormat.MachineRole) &&
            string.Equals(context.User.FindFirstValue(ClaimTypes.NameIdentifier), resource, StringComparison.Ordinal))
        {
            context.Succeed(requirement);
        }

        return Task.CompletedTask;
    }
}