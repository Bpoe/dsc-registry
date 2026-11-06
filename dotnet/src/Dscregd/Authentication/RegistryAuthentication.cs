namespace Dscregd.Authentication;

using Dscregd.Services;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Authorization;
using Microsoft.Extensions.DependencyInjection;

public static class RegistryAuthentication
{
    public const string ApiKeyScheme = "ApiKey";
    public const string RegistrationScheme = "Bearer";

    public static IServiceCollection AddRegistryAuthentication(this IServiceCollection services)
    {
        services.AddScoped<CredentialService>();
        services.AddScoped<MachineService>();
        services.AddAuthentication(ApiKeyScheme)
            .AddScheme<AuthenticationSchemeOptions, ApiKeyAuthenticationHandler>(ApiKeyScheme, null)
            .AddScheme<AuthenticationSchemeOptions, RegistrationAuthenticationHandler>(RegistrationScheme, null);
        services.AddAuthorization(options =>
        {
            options.AddPolicy("Admin", policy => policy.AddAuthenticationSchemes(ApiKeyScheme)
                .RequireAuthenticatedUser().RequireRole(CredentialFormat.AdminRole));
            options.AddPolicy("Machine", policy => policy.AddAuthenticationSchemes(ApiKeyScheme)
                .RequireAuthenticatedUser().RequireRole(CredentialFormat.MachineRole));
            options.AddPolicy("Registration", policy => policy.AddAuthenticationSchemes(RegistrationScheme)
                .RequireAuthenticatedUser().RequireRole(CredentialFormat.RegistrationRole));
        });
        services.AddSingleton<IAuthorizationHandler, MachineOwnerAuthorizationHandler>();
        return services;
    }
}