// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text;
using System.Text.Json;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public class ServerTests
{
    private const string HelloLine = """{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":1}}""";

    [Fact]
    public async Task HelloThenCall()
    {
        var (code, lines) = await Serve(HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"echo","params":{"protocol":5}}""" + "\n");

        Assert.Equal(Server.ExitOk, code);
        Assert.Equal(2, lines.Count);
        var hello = lines[0].GetProperty("result");
        Assert.Equal(1, lines[0].GetProperty("id").GetInt64());
        Assert.Equal(Server.ProtocolVersion, hello.GetProperty("protocol").GetInt32());
        Assert.False(string.IsNullOrEmpty(hello.GetProperty("version").GetString()));
        Assert.StartsWith(".NET", hello.GetProperty("runtime").GetString(), StringComparison.Ordinal);
        Assert.Equal(2, lines[1].GetProperty("id").GetInt64());
        Assert.Equal(5, lines[1].GetProperty("result").GetProperty("protocol").GetInt32());
    }

    [Fact]
    public async Task HelloAnswersItsOwnProtocol()
    {
        var (code, lines) = await Serve("""{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":99}}""" + "\n", keepOpen: false);

        Assert.Equal(Server.ExitOk, code);
        Assert.Equal(Server.ProtocolVersion, lines.Single().GetProperty("result").GetProperty("protocol").GetInt32());
    }

    public static TheoryData<string, int, string, long?> Failures => new()
    {
        { """{"jsonrpc":"2.0","id":1,"method":"echo"}""" + "\n", Server.ExitProtocolError, ErrorCodes.InvalidRequest, 1 },
        { """{"jsonrpc":"2.0","id":1,"method":"hello","params":{}}""" + "\n", Server.ExitProtocolError, ErrorCodes.InvalidRequest, 1 },
        { "{oops\n", Server.ExitProtocolError, ErrorCodes.InvalidRequest, null },
        { new string('x', LineReader.MaxLineBytes + 1) + "\n", Server.ExitProtocolError, ErrorCodes.InvalidRequest, null },
        { HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"nope"}""" + "\n", Server.ExitOk, ErrorCodes.UnknownMethod, 2 },
        { HelloLine + "\n" + "{oops\n", Server.ExitProtocolError, ErrorCodes.InvalidRequest, null },
        { HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"fail"}""" + "\n", Server.ExitOk, ErrorCodes.NotDotnet, 2 },
        { HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"crash"}""" + "\n", Server.ExitOk, ErrorCodes.HelperFailed, 2 },
    };

    [Theory]
    [MemberData(nameof(Failures))]
    public async Task AnswersErrors(string input, int wantCode, string wantError, long? wantId)
    {
        var (code, lines) = await Serve(input);

        Assert.Equal(wantCode, code);
        var last = lines[^1];
        Assert.Equal(wantError, last.GetProperty("error").GetProperty("data").GetProperty("code").GetString());
        Assert.Equal(-32000, last.GetProperty("error").GetProperty("code").GetInt32());
        var id = last.GetProperty("id");
        Assert.Equal(wantId, id.ValueKind == JsonValueKind.Null ? null : id.GetInt64());
    }

    [Fact]
    public async Task CrashMessageIsBounded()
    {
        var (_, lines) = await Serve(HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"crash"}""" + "\n");

        var message = lines[^1].GetProperty("error").GetProperty("message").GetString()!;
        Assert.StartsWith("InvalidOperationException: ", message, StringComparison.Ordinal);
        Assert.Equal(500, message.Length);
    }

    [Fact]
    public async Task EmptyInputExitsQuietly()
    {
        var (code, lines) = await Serve("", keepOpen: false);

        Assert.Equal(Server.ExitOk, code);
        Assert.Empty(lines);
    }

    [Fact]
    public async Task EndOfInputCancelsTheCallWithoutAnAnswer()
    {
        var input = new GatedStream(Encoding.UTF8.GetBytes(HelloLine + "\n" + """{"jsonrpc":"2.0","id":2,"method":"wait"}""" + "\n"));
        var output = new MemoryStream();
        var started = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var methods = Methods();
        methods["wait"] = async (_, notifier, ct) =>
        {
            notifier.Notify("tick", "before", ProtocolJson.Default.String);
            started.SetResult();
            await Task.Delay(Timeout.Infinite, ct);
            return new Reply("never", ProtocolJson.Default.String);
        };
        var run = new Server(new LineReader(input), new MessageWriter(output), methods).RunAsync();

        await started.Task.WaitAsync(TestContext.Current.CancellationToken);
        input.End();
        var code = await run.WaitAsync(TestContext.Current.CancellationToken);

        Assert.Equal(Server.ExitOk, code);
        var lines = Lines(output);
        Assert.Equal(2, lines.Count);
        Assert.Equal("tick", lines[1].GetProperty("method").GetString());
        Assert.False(lines[1].TryGetProperty("id", out _));
    }

    [Fact]
    public void ProcessesExcludeSelfDeduplicateAndSort()
    {
        var result = ProcessesMethod.List(new FakeProcesses([30, 7, 30, 99, 0, -1, 12]), selfPid: 99);

        Assert.Equal([7, 12, 30], result.Processes.Select(p => p.Pid));
    }

    private static Dictionary<string, MethodHandler> Methods() => new(StringComparer.Ordinal)
    {
        ["echo"] = (p, _, _) => Task.FromResult(new Reply(ProtocolJson.ParseParams(p, ProtocolJson.Default.HelloParams), ProtocolJson.Default.HelloParams)),
        ["fail"] = (_, _, _) => throw new HelperException(ErrorCodes.NotDotnet, "no"),
        ["crash"] = (_, _, _) => throw new InvalidOperationException(new string('y', 1000)),
    };

    /// <summary>
    /// Serves input; with keepOpen, the input stays open after it (as eyedbg keeps stdin open
    /// until it has the answer: its end would cancel the call).
    /// </summary>
    private static async Task<(int Code, List<JsonElement> Lines)> Serve(string input, bool keepOpen = true)
    {
        var output = new MemoryStream();
        var stream = new GatedStream(Encoding.UTF8.GetBytes(input));
        if (!keepOpen)
        {
            stream.End();
        }

        var server = new Server(new LineReader(stream), new MessageWriter(output), Methods());
        var code = await server.RunAsync().WaitAsync(TestContext.Current.CancellationToken);
        stream.End();
        return (code, Lines(output));
    }

    private static List<JsonElement> Lines(MemoryStream output)
    {
        var text = Encoding.UTF8.GetString(output.ToArray());
        Assert.DoesNotContain('\r', text);
        return text.Split('\n', StringSplitOptions.RemoveEmptyEntries).Select(l => JsonDocument.Parse(l).RootElement.Clone()).ToList();
    }

    private sealed class FakeProcesses(IEnumerable<int> pids) : IProcessSource
    {
        public IEnumerable<int> PublishedProcesses() => pids;
    }

    /// <summary>Yields its bytes, then blocks reads until End (then reads return 0).</summary>
    private sealed class GatedStream(byte[] data) : Stream
    {
        private readonly TaskCompletionSource _end = new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _position;

        public override bool CanRead => true;

        public override bool CanSeek => false;

        public override bool CanWrite => false;

        public override long Length => throw new NotSupportedException();

        public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }

        public void End() => _end.TrySetResult();

        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            if (_position < data.Length)
            {
                var n = Math.Min(buffer.Length, data.Length - _position);
                data.AsMemory(_position, n).CopyTo(buffer);
                _position += n;
                return n;
            }

            await _end.Task.WaitAsync(cancellationToken);
            return 0;
        }

        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException();

        public override void Flush()
        {
        }

        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

        public override void SetLength(long value) => throw new NotSupportedException();

        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }
}
