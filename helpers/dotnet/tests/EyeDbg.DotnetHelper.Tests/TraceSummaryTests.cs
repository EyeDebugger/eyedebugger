// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics.Tracing;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Tests;

[Collection(nameof(SelfTracing))]
public sealed class TraceSummaryTests : IDisposable
{
    private readonly string _dir = Directory.CreateTempSubdirectory("eyedbg-trace-summary-tests-").FullName;

    public void Dispose() => Directory.Delete(_dir, recursive: true);

    public static TheoryData<string, string, int, long, string?> ParamCases => new()
    {
        { "trace", "s.etlx", 1, 1000, null },
        { "trace", "s.etlx", TraceSummaryMethod.MaxTop, TraceSummaryMethod.MaxTimeoutMs, null },
        { "relative", "s.etlx", 10, 1000, "path must be absolute" },
        { "missing", "s.etlx", 10, 1000, "no trace file at " },
        { "trace", "relative", 10, 1000, "scratch must be absolute" },
        { "trace", "exists", 10, 1000, "scratch exists: " },
        { "trace", "new-exists", 10, 1000, "scratch exists: " },
        { "trace", "s.etlx", 0, 1000, "top must be 1 to 1000" },
        { "trace", "s.etlx", TraceSummaryMethod.MaxTop + 1, 1000, "top must be 1 to 1000" },
        { "trace", "s.etlx", 10, 999, "timeoutMs must be 1000 to 3600000" },
        { "trace", "s.etlx", 10, TraceSummaryMethod.MaxTimeoutMs + 1, "timeoutMs must be 1000 to 3600000" },
    };

    [Theory]
    [MemberData(nameof(ParamCases))]
    public void ValidatesParams(string path, string scratch, int top, long timeoutMs, string? wantError)
    {
        var trace = Path.Combine(_dir, "t.nettrace");
        File.WriteAllBytes(trace, [1]);
        File.WriteAllBytes(Path.Combine(_dir, "exists"), [1]);
        File.WriteAllBytes(Path.Combine(_dir, "new-exists.new"), [1]);
        var p = new TraceSummaryParams
        {
            Path = path switch
            {
                "trace" => trace,
                "relative" => "t.nettrace",
                _ => Path.Combine(_dir, "none.nettrace"),
            },
            Scratch = scratch == "relative" ? "s.etlx" : Path.Combine(_dir, scratch),
            Top = top,
            TimeoutMs = timeoutMs,
        };

        if (wantError is null)
        {
            Assert.Equal(p, TraceSummaryMethod.Validate(p));
            return;
        }

        var e = Assert.Throws<HelperException>(() => TraceSummaryMethod.Validate(p));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.StartsWith(wantError, e.Message, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData("text")]
    [InlineData("empty")]
    [InlineData("garbage")]
    [InlineData("cut")]
    public async Task RefusesUnreadableTraces(string what)
    {
        var path = Path.Combine(_dir, what + ".nettrace");
        switch (what)
        {
            case "text":
                await File.WriteAllTextAsync(path, "hello, this is not a trace\n", TestContext.Current.CancellationToken);
                break;
            case "empty":
                await File.WriteAllBytesAsync(path, [], TestContext.Current.CancellationToken);
                break;
            case "garbage":
                await File.WriteAllBytesAsync(path, [.. Enumerable.Range(0, 64 * 1024).Select(i => (byte)(i * 7919 >> 3))], TestContext.Current.CancellationToken);
                break;
            default:
                // The first 4 KiB of a real trace: cut off, as when the process was killed.
                using (var load = Load.Spinning())
                {
                    var (full, _) = await Load.TraceSelfAsync(_dir, "cpu", TestContext.Current.CancellationToken);
                    var bytes = await File.ReadAllBytesAsync(full, TestContext.Current.CancellationToken);
                    await File.WriteAllBytesAsync(path, bytes[..4096], TestContext.Current.CancellationToken);
                }

                break;
        }

        var scratch = Path.Combine(_dir, "s.etlx");
        var e = Assert.Throws<HelperException>(() => TraceSummary.Run(Params(path, scratch), TestContext.Current.CancellationToken));

        Assert.Equal(ErrorCodes.TraceUnsupported, e.Code);
        Assert.False(File.Exists(scratch));
        Assert.False(File.Exists(scratch + ".new"));
        Assert.False(File.Exists(path + ".etlx"));
    }

    public static TheoryData<Exception, bool, string?> Failures => new()
    {
        { new FormatException("secret-format"), false, ErrorCodes.TraceUnsupported },
        { new FormatException("secret-format2"), true, ErrorCodes.TraceUnsupported },
        { new EndOfStreamException(), false, ErrorCodes.TraceUnsupported },
        { new EndOfStreamException(), true, ErrorCodes.TraceUnsupported },
        { new InvalidDataException(), false, ErrorCodes.TraceUnsupported },
        { new IOException("secret-io"), false, ErrorCodes.TraceUnsupported },
        { new IOException("No space left on device"), true, null }, // the scratch file: HELPER_FAILED with its message
        { new UnauthorizedAccessException("secret-access"), false, ErrorCodes.TraceUnsupported }, // the FILE: can't read it
        { new UnauthorizedAccessException("Access to the path is denied"), true, null }, // the scratch file
        { new ArgumentOutOfRangeException("secret"), false, ErrorCodes.HelperFailed },
        { new OperationCanceledException(), false, null },
        { new HelperException(ErrorCodes.TraceUnsupported, "m"), false, null },
    };

    [Theory]
    [MemberData(nameof(Failures))]
    public void MapsParserFailures(Exception e, bool writing, string? wantCode)
    {
        var got = TraceSummary.Unreadable(e, "/t/x.nettrace", writing);

        Assert.Equal(wantCode, got?.Code);
        if (got is not null)
        {
            // The parser's own message can quote the file: never passed on.
            Assert.DoesNotContain(e.Message, got.Message, StringComparison.Ordinal);
        }

        if (wantCode == ErrorCodes.HelperFailed)
        {
            Assert.Equal("the trace parser failed: ArgumentOutOfRangeException", got!.Message);
        }

        if (e is UnauthorizedAccessException && got is not null)
        {
            Assert.Equal("can't read /t/x.nettrace: permission denied", got.Message);
            Assert.False(string.IsNullOrEmpty(got.Hint));
        }
    }

    [Fact]
    public async Task AFileYouCantReadIsUnsupported()
    {
        if (OperatingSystem.IsWindows())
        {
            Assert.Skip("Unix modes");
            return;
        }

        string path;
        using (var load = Load.Spinning())
        {
            (path, _) = await Load.TraceSelfAsync(_dir, "cpu", TestContext.Current.CancellationToken);
        }

        File.SetUnixFileMode(path, UnixFileMode.None);

        var e = Assert.Throws<HelperException>(() => TraceSummary.Run(Params(path, Path.Combine(_dir, "s.etlx")), TestContext.Current.CancellationToken));

        Assert.Equal(ErrorCodes.TraceUnsupported, e.Code);
        Assert.Equal("can't read " + path + ": permission denied", e.Message);
        File.SetUnixFileMode(path, UnixFileMode.UserRead | UnixFileMode.UserWrite);
    }

    [Fact]
    public async Task AReadableTraceWithNothingInItIsAnEmptySummary()
    {
        // A session with a provider nobody writes to: a well-formed trace without samples, GCs or
        // allocation ticks — what a gc trace of an idle process can be (M8-Q13).
        var path = Path.Combine(_dir, "quiet.nettrace");
        using (var process = System.Diagnostics.Process.GetCurrentProcess())
        {
            var file = TraceFile.Create(path);
            using var session = await EventPipeStart.StartAsync(
                Environment.ProcessId,
                process,
                [new EventPipeProvider("EyeDbg-Tests-Nobody", EventLevel.Informational, 0)],
                requestRundown: false,
                circularBufferMB: 16,
                TestContext.Current.CancellationToken);
            var copier = StreamCopier.Start(session.EventStream, file, long.MaxValue);
            await session.StopAsync(TestContext.Current.CancellationToken);
            await copier.Done.WaitAsync(TestContext.Current.CancellationToken);
        }

        var r = TraceSummary.Run(Params(path, Path.Combine(_dir, "s.etlx")), TestContext.Current.CancellationToken);

        Assert.Null(r.Cpu);
        Assert.Null(r.Gc);
        Assert.Null(r.Allocations);
        Assert.Equal(0, r.EventsLost);
        Assert.Equal([path], Directory.GetFiles(_dir));
    }

    [Fact]
    public async Task SummarizesACpuTrace()
    {
        string path;
        using (var load = Load.Spinning())
        {
            (path, _) = await Load.TraceSelfAsync(_dir, "cpu", TestContext.Current.CancellationToken);
        }

        var scratch = Path.Combine(_dir, "s.etlx");
        var r = TraceSummary.Run(Params(path, scratch), TestContext.Current.CancellationToken);

        Assert.NotNull(r.Cpu);
        Assert.InRange(r.DurationMs, 500, 60_000);
        Assert.True(r.Cpu.Managed > 0);
        Assert.Equal(r.Cpu.Samples, r.Cpu.Managed + r.Cpu.Other);
        Assert.Contains(r.Cpu.Exclusive.Take(5), m => m.Method == Load.HotMethod && m.Module == "EyeDbg.DotnetHelper.Tests");
        Assert.Contains(r.Cpu.Inclusive, m => m.Method == Load.HotMethod);
        Assert.Null(r.Allocations);

        // The ETLX scratch file is gone, and nothing was put next to the trace.
        Assert.False(File.Exists(scratch));
        Assert.False(File.Exists(scratch + ".new"));
        Assert.Equal([path], Directory.GetFiles(_dir));
    }

    [Fact]
    public async Task SummarizesAGcTrace()
    {
        string path;
        using (var load = Load.Allocating())
        {
            (path, _) = await Load.TraceSelfAsync(_dir, "gc", TestContext.Current.CancellationToken);
        }

        var r = TraceSummary.Run(Params(path, Path.Combine(_dir, "s.etlx")), TestContext.Current.CancellationToken);

        Assert.Null(r.Cpu);
        Assert.NotNull(r.Gc);
        Assert.True(r.Gc.Gen0 >= 1);
        Assert.True(r.Gc.PauseTotalMs >= r.Gc.PauseMaxMs);
        Assert.NotNull(r.Allocations);
        Assert.Contains(r.Allocations.Types, t => t.Name == "System.Byte[]");
        Assert.Equal([path], Directory.GetFiles(_dir));
    }

    [Fact]
    public async Task CancelingEndsTheSummary()
    {
        string path;
        using (var load = Load.Spinning())
        {
            (path, _) = await Load.TraceSelfAsync(_dir, "cpu", TestContext.Current.CancellationToken);
        }

        using var canceled = new CancellationTokenSource();
        await canceled.CancelAsync();

        Assert.ThrowsAny<OperationCanceledException>(() => TraceSummary.Run(Params(path, Path.Combine(_dir, "s.etlx")), canceled.Token));
        Assert.Equal([path], Directory.GetFiles(_dir));
    }

    [Fact]
    public void AFifoIsNeverOpened()
    {
        if (OperatingSystem.IsWindows())
        {
            Assert.Skip("FIFOs are Unix");
        }

        var fifo = Path.Combine(_dir, "fifo.nettrace");
        using (var mkfifo = System.Diagnostics.Process.Start("mkfifo", fifo))
        {
            mkfifo.WaitForExit();
            Assert.Equal(0, mkfifo.ExitCode);
        }

        // A FIFO reports no length: refused before any open (which would block).
        var e = Assert.Throws<HelperException>(() => TraceSummary.Run(Params(fifo, Path.Combine(_dir, "s.etlx")), TestContext.Current.CancellationToken));
        Assert.Equal(ErrorCodes.TraceUnsupported, e.Code);
    }

    private static TraceSummaryParams Params(string path, string scratch) => new() { Path = path, Scratch = scratch, Top = 10, TimeoutMs = 60_000 };
}
