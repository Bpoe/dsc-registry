namespace Dscregd.Tests;

using System.Text.Json;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class JsonBehaviorTests
{
    [TestMethod]
    [DataRow("1", "1.00e0")]
    [DataRow("1000", "10e2")]
    [DataRow("-0.000e-9999", "0")]
    [DataRow("0.00120", "120e-5")]
    [DataRow("-123000.000", "-123e3")]
    [DataRow("9007199254740993123456789", "9007199254740993123456789.000")]
    public void NativeJsonComparisonPreservesNumericPrecision(string left, string right)
    {
        using var first = JsonDocument.Parse(left);
        using var second = JsonDocument.Parse(right);
        Assert.IsTrue(JsonElement.DeepEquals(first.RootElement, second.RootElement));
    }

    [TestMethod]
    public void NativeJsonComparisonIgnoresObjectPropertyOrder()
    {
        using var first = JsonDocument.Parse("""{"b":[2,3],"a":1}""");
        using var second = JsonDocument.Parse("""{"a":1.0,"b":[2,3]}""");
        Assert.IsTrue(JsonElement.DeepEquals(first.RootElement, second.RootElement));
    }
}