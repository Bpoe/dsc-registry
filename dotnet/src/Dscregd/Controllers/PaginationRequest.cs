namespace Dscregd.Controllers;

using System.ComponentModel.DataAnnotations;

public class PaginationRequest
{
    [Range(1, 1000)]
    public int Limit { get; set; } = 100;

    public string Cursor { get; set; } = "";
}