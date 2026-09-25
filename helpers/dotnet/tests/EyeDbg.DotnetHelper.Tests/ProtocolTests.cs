// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text;
using System.Text.Json;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public class ProtocolTests
{
    public static TheoryData<byte[], string?, long?> Requests => new()
    {
        { """{"jsonrpc":"2.0","id":7,"method":"m","params":{"a":1}}"""u8.ToArray(), null, 7 },
        { """{"jsonrpc":"2.0","id":7,"method":"m"}"""u8.ToArray(), null, 7 },
        { "not json"u8.ToArray(), "invalid JSON", null },
        { [.. "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\""u8, 0xff, .. "\"}"u8], "invalid JSON", null },
        { """{"jsonrpc":"2.0","method":"m","params":{"s":"ÿ"}}"""u8.ToArray(), "a request needs an integer id", null },
        { "[1]"u8.ToArray(), "a request must be a JSON object", null },
        { """{"jsonrpc":"1.0","id":3,"method":"m"}"""u8.ToArray(), "jsonrpc must be \"2.0\"", 3 },
        { """{"jsonrpc":"2.0","id":"3","method":"m"}"""u8.ToArray(), "a request needs an integer id", null },
        { """{"jsonrpc":"2.0","id":1.5,"method":"m"}"""u8.ToArray(), "a request needs an integer id", null },
        { """{"jsonrpc":"2.0","id":4,"method":""}"""u8.ToArray(), "a request needs a method", 4 },
        { """{"jsonrpc":"2.0","id":4,"method":5}"""u8.ToArray(), "a request needs a method", 4 },
    };

    [Theory]
    [MemberData(nameof(Requests))]
    public void ParsesRequests(byte[] line, string? wantError, long? wantId)
    {
        var (request, id, error) = Request.Parse(line);

        Assert.Equal(wantError, error);
        Assert.Equal(wantId, id);
        Assert.Equal(wantError is null, request is not null);
    }

    [Fact]
    public void ParamsKeepAfterParse()
    {
        var (request, _, _) = Request.Parse("""{"jsonrpc":"2.0","id":1,"method":"counters","params":{"pid":5,"intervalSec":2}}"""u8.ToArray());

        var p = ProtocolJson.ParseParams(request!.Value.Params, ProtocolJson.Default.CountersParams);

        Assert.Equal(new CountersParams { Pid = 5, IntervalSec = 2, DurationMs = 0 }, p);
    }

    [Theory]
    [InlineData("""{"pid":1}""")]
    [InlineData("""{"pid":"1","intervalSec":1}""")]
    [InlineData("""[]""")]
    public void BadParamsAreInvalidRequest(string json)
    {
        using var doc = JsonDocument.Parse(json);

        var e = Assert.Throws<HelperException>(() => ProtocolJson.ParseParams(doc.RootElement, ProtocolJson.Default.CountersParams));

        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
    }

    [Fact]
    public void EncodesOneLineWithoutCarriageReturns()
    {
        var line = MessageWriter.Encode(w => w.WriteString("s", "a\r\nb"));

        Assert.NotNull(line);
        Assert.Equal((byte)'\n', line[^1]);
        Assert.Equal(1, line.Count(b => b == (byte)'\n'));
        Assert.DoesNotContain((byte)'\r', line);
        Assert.Equal("""{"jsonrpc":"2.0","s":"a\r\nb"}""" + "\n", Encoding.UTF8.GetString(line));
    }

    [Fact]
    public void EncodeRefusesTooLargeMessages()
    {
        var fits = new string('x', MessageWriter.MaxMessageSize - """{"jsonrpc":"2.0","s":""}""".Length - 1);

        Assert.Equal(MessageWriter.MaxMessageSize, MessageWriter.Encode(w => w.WriteString("s", fits))!.Length);
        Assert.Null(MessageWriter.Encode(w => w.WriteString("s", fits + "x")));
    }

    [Fact]
    public void TooLargeResultBecomesHelperFailed()
    {
        var output = new MemoryStream();
        var writer = new MessageWriter(output);

        writer.WriteResult(9, new Reply(new string('x', MessageWriter.MaxMessageSize), ProtocolJson.Default.String));

        using var doc = JsonDocument.Parse(output.ToArray());
        Assert.Equal(9, doc.RootElement.GetProperty("id").GetInt64());
        Assert.Equal(ErrorCodes.HelperFailed, doc.RootElement.GetProperty("error").GetProperty("data").GetProperty("code").GetString());
    }

    [Fact]
    public void ErrorsHaveTheDaemonsShape()
    {
        var output = new MemoryStream();

        new MessageWriter(output).WriteError(3, new HelperException(ErrorCodes.NotDotnet, "msg", "hint"));

        Assert.Equal(
            """{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"msg","data":{"code":"NOT_DOTNET","message":"msg","hint":"hint"}}}""" + "\n",
            Encoding.UTF8.GetString(output.ToArray()));
    }
}
