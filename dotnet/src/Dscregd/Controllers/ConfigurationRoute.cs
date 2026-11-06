namespace Dscregd.Controllers;

using System.ComponentModel.DataAnnotations;
using Dscregd.Data;

public class ConfigurationRoute
{
    public Guid Id { get; set; }

    [RegularExpression(DesiredConfiguration.NamePattern)]
    public string Name { get; set; } = "";
}