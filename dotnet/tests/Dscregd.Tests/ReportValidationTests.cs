namespace Dscregd.Tests;

using System;
using System.Collections.Generic;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using Dscregd.Reports;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class ReportValidationTests
{
    public const string Success = """
        {
         "schemaVersion":1, "configuration":"../custom/docker.yaml", "operation":"test",
         "parameters":"C:\\Shared Inputs\\values.yaml",
         "startedAt":"2026-10-18T12:00:00Z", "finishedAt":"2026-10-18T12:00:00.005Z",
         "durationMs":5, "outcome":"succeeded", "exitCode":null,
         "dscResult":{"results":[{"inDesiredState":false,"number":9007199254740993123456789,"huge":1e999999}],"hadErrors":true},
         "error":null,"stderr":"","future":{"value":"\u0000"}
        }
        """;

    public static string Wrap(string result, string? revision = null, string? uploadId = null, string name = "docker")
    {
        revision ??= "11111111-1111-4111-8111-111111111111";
        uploadId ??= "22222222-2222-4222-8222-222222222222";
        return $$"""{"configurationName":"{{name}}","configurationRevision":"{{revision}}","uploadId":"{{uploadId}}","result":{{result}}}""";
    }

    [TestMethod]
    [DataRow("set")]
    [DataRow("test")]
    [DataRow("export")]
    public void SuccessIsOpaqueAndPreservesBytes(string operation)
    {
        var result = Success.Replace("\"operation\":\"test\"", "\"operation\":\"" + operation + "\"", StringComparison.Ordinal);
        var upload = ParseUpload(Encoding.UTF8.GetBytes(Wrap(result)));
        Assert.AreEqual(operation, upload.Operation);
        Assert.AreEqual("succeeded", upload.Outcome);
        Assert.IsNull(upload.ErrorKind);
        CollectionAssert.AreEqual(Encoding.UTF8.GetBytes(result), upload.Result);
    }

    [TestMethod]
    [DataRow("input", "failed")]
    [DataRow("start", "failed")]
    [DataRow("dsc", "failed")]
    [DataRow("output", "failed")]
    [DataRow("exit", "failed")]
    [DataRow("canceled", "canceled")]
    public void FailureAcceptsActualNullAndMissingFields(string kind, string outcome)
    {
        foreach (var optional in new[] { "", """
            ,"operation":null,"parameters":null,"inputHash":null,"exitCode":null,"dscResult":null,"stderr":null
            """ })
        {
            var result = $$"""
                {"schemaVersion":1,"configuration":"any-path","startedAt":"2026-10-18T12:00:00Z",
                 "finishedAt":"2026-10-18T12:00:00Z","durationMs":9223372036854775807,
                 "outcome":"{{outcome}}","error":{"kind":"{{kind}}","message":"","future":42}{{optional}}}
                """;
            var upload = ParseUpload(Encoding.UTF8.GetBytes(Wrap(result)));
            Assert.AreEqual(outcome, upload.Outcome);
            Assert.AreEqual(kind, upload.ErrorKind);
            Assert.IsNull(upload.Operation);
            CollectionAssert.AreEqual(Encoding.UTF8.GetBytes(result), upload.Result);
        }
    }

    [TestMethod]
    public void InvalidSummaryFieldsAreRejected()
    {
        var changes = new Dictionary<string, string>
        {
            ["\"schemaVersion\":1"] = "\"schemaVersion\":1.0",
            ["\"configuration\":\"../custom/docker.yaml\""] = "\"configuration\":null",
            ["\"operation\":\"test\""] = "\"operation\":\"get\"",
            ["\"parameters\":\"C:\\\\Shared Inputs\\\\values.yaml\""] = "\"parameters\":[]",
            ["\"finishedAt\":\"2026-10-18T12:00:00.005Z\""] = "\"finishedAt\":\"2026-10-18T11:59:59Z\"",
            ["\"durationMs\":5"] = "\"durationMs\":9223372036854775808",
            ["\"outcome\":\"succeeded\""] = "\"outcome\":\"compliant\"",
            ["\"exitCode\":null"] = "\"exitCode\":1",
            ["\"error\":null"] = "\"error\":{\"kind\":\"start\",\"message\":\"failure\"}",
            ["\"stderr\":\"\""] = "\"stderr\":true"
        };
        foreach (var change in changes)
        {
            var result = Success.Replace(change.Key, change.Value, StringComparison.Ordinal);
            Assert.AreNotEqual(Success, result);
            var error = Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(Wrap(result))));
            Assert.AreEqual("invalid_report", error.Code);
        }
    }

    [TestMethod]
    [DataRow("\"durationMs\":5", "\"durationMs\":null")]
    [DataRow("\"durationMs\":5", "\"durationMs\":5e0")]
    [DataRow("\"durationMs\":5", "\"durationMs\":-1")]
    [DataRow("\"exitCode\":null", "\"exitCode\":-1")]
    [DataRow("\"exitCode\":null", "\"exitCode\":9223372036854775808")]
    [DataRow("\"error\":null", "\"error\":{\"kind\":\"unknown\",\"message\":\"\"}")]
    [DataRow("\"schemaVersion\":1", "\"schemaVersion\":1,\"schema\\u0056ersion\":1")]
    [DataRow("\"value\":\"\\u0000\"", "\"value\":1,\"value\":2")]
    public void AmbiguousOrInvalidJsonIsRejected(string original, string replacement)
    {
        var result = Success.Replace(original, replacement, StringComparison.Ordinal);
        Assert.AreNotEqual(Success, result);
        Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(Wrap(result))));
    }

    [TestMethod]
    public void InvalidUtf8DepthAndWrapperAreRejected()
    {
        var bytes = Encoding.UTF8.GetBytes(Wrap(Success));
        var marker = Array.IndexOf(bytes, (byte)'f');
        bytes[marker] = 0xff;
        Assert.ThrowsExactly<DomainException>(() => ParseUpload(bytes));
        var deep = Success.Replace("\"value\":\"\\u0000\"",
            "\"value\":" + new string('[', 130) + "0" + new string(']', 130), StringComparison.Ordinal);
        Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(Wrap(deep))));
        foreach (var invalid in new[]
        {
            "null", "[]", "{}", Wrap(Success) + "{}",
            Wrap(Success).Replace("\"configurationName\":", "\"machineId\":", StringComparison.Ordinal),
            Wrap(Success).Replace("\"configurationName\":\"docker\"", "\"configurationName\":\"../docker\"", StringComparison.Ordinal),
            Wrap(Success).Replace("\"uploadId\":", "\"uploadId\":null,\"uploadId\":", StringComparison.Ordinal)
        })
        {
            Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(invalid)));
        }
    }

    [TestMethod]
    [DataRow("2026-10-18T12:00:00Z", true)]
    [DataRow("2026-10-18T1:00:00Z", false)]
    [DataRow("2026-10-18T12:00:00,1234567891Z", false)]
    [DataRow("0000-02-29T00:00:00Z", false)]
    [DataRow("0001-01-01T00:00:00Z", false)]
    [DataRow("0001-01-01T00:00:00.0000000001Z", false)]
    [DataRow("2026-02-29T00:00:00Z", false)]
    [DataRow("2026-10-18T24:00:00Z", false)]
    [DataRow("2026-10-18T12:00:60Z", false)]
    [DataRow("2026-10-18T12:00:00z", false)]
    [DataRow("2026-10-18T12:00:00+00:00", true)]
    [DataRow("2026-10-18T12:00:00.123456789Z", true)]
    public void TimestampValidationUsesNativeDateTimeOffsetSemantics(string timestamp, bool valid)
    {
        var result = Success.Replace("2026-10-18T12:00:00Z", timestamp, StringComparison.Ordinal)
            .Replace("2026-10-18T12:00:00.005Z", timestamp, StringComparison.Ordinal);
        if (valid)
        {
            var upload = ParseUpload(Encoding.UTF8.GetBytes(Wrap(result)));
            Assert.AreEqual(timestamp, upload.StartedAt);
            Assert.AreEqual(timestamp, upload.FinishedAt);
        }

        else
        {
            Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(Wrap(result))));
        }
    }

    [TestMethod]
    [DataRow("", true)]
    [DataRow("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true)]
    [DataRow("sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", false)]
    [DataRow("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false)]
    [DataRow("sha256:abc", false)]
    public void InputHashIsOptionalButValidated(string inputHash, bool valid)
    {
        var result = Success.Replace("\"future\":", "\"inputHash\":\"" + inputHash + "\",\"future\":", StringComparison.Ordinal);
        if (valid)
        {
            Assert.AreEqual(inputHash, ParseUpload(Encoding.UTF8.GetBytes(Wrap(result))).InputHash);
        }

        else
        {
            Assert.ThrowsExactly<DomainException>(() => ParseUpload(Encoding.UTF8.GetBytes(Wrap(result))));
        }
    }

    [TestMethod]
    [DataRow("\"value\":\"\\u0000\"", "\"value\":\"\\ud800\"")]
    [DataRow("\"value\":\"\\u0000\"", "\"value\":\"\\udc00\"")]
    [DataRow("\"schemaVersion\":1", "\"schemaVersion\":1,\"SchemaVersion\":2")]
    [DataRow("\"schemaVersion\":1", "\"schemaVersion\":1,\"\\u017fchemaVersion\":2")]
    [DataRow("\"future\":", "\"Error\":null,\"future\":")]
    public void NativeDeserializerSkipsUnknownReportFieldsWithoutRewritingThem(string original, string replacement)
    {
        var result = Success.Replace(original, replacement, StringComparison.Ordinal);
        var upload = ParseUpload(Encoding.UTF8.GetBytes(Wrap(result)));
        CollectionAssert.AreEqual(Encoding.UTF8.GetBytes(result), upload.Result);
    }

    private static ReportUpload ParseUpload(byte[] body)
    {
        var json = new JsonSerializerOptions(JsonSerializerDefaults.Web)
        {
            PropertyNameCaseInsensitive = false,
            AllowDuplicateProperties = false,
            RespectNullableAnnotations = true,
            UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow,
            MaxDepth = 128
        };
        try
        {
            var request = JsonSerializer.Deserialize<ReportSubmission>(body, json)
                ?? throw new DomainException(400, "invalid_report", "Missing report");
            return ReportUpload.FromSubmission(request, json);
        }
        catch (JsonException)
        {
            throw new DomainException(400, "invalid_report", "Invalid JSON report");
        }
    }
}