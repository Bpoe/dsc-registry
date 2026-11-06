namespace Dscregd.Tests;

using System.Reflection;
using Dscregd.Controllers;
using Dscregd.Data;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Mvc;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class LifecycleTests
{
    [TestMethod]
    public void ControllersUseFrameworkBaseAndComposedDependencies()
    {
        var controllers = typeof(ConfigurationsController).Assembly.GetTypes()
            .Where(type => type.IsClass && typeof(ControllerBase).IsAssignableFrom(type))
            .ToArray();
        Assert.IsTrue(controllers.Length > 0);
        foreach (var controller in controllers)
        {
            Assert.AreEqual(typeof(ControllerBase), controller.BaseType, controller.Name);
            Assert.IsFalse(controller.IsSealed, controller.Name);
            Assert.IsNotNull(controller.GetCustomAttribute<ApiControllerAttribute>(inherit: false), controller.Name);
        }
    }

    [TestMethod]
    public async Task NativeHostedLifecycleInitializesDependenciesBeforeKestrelServesRequests()
    {
        Assert.IsTrue(typeof(IHostedLifecycleService).IsAssignableFrom(typeof(InitializationService)));
        await using var factory = new RegistryFactory();
        await factory.InitializeAsync();
        StringAssert.Contains(factory.Services.GetRequiredService<IServer>().GetType().Name, "Kestrel");
        using var response = await factory.Client.GetAsync("/health/ready");
        response.EnsureSuccessStatusCode();
        using var scope = factory.Services.CreateScope();
        Assert.AreEqual(1, await scope.ServiceProvider.GetRequiredService<RegistryDbContext>().Admins.CountAsync());
    }
}