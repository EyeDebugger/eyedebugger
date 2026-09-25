// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics.Tracing;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Tests;

[Collection(nameof(SelfTracing))]
public sealed class TraceCollectorTests : IDisposable
{
    private readonly string _dir = Directory.CreateTempSubdirectory("eyedbg-trace-tests-").FullName;

    public void Dispose() => Directory.Delete(_dir, recursive: true);

    public static TheoryData<string, string, bool> Profiles => new()
    {
        {
            "cpu",
            "Microsoft-DotNETCore-SampleProfiler:Informational:0x0;Microsoft-Windows-DotNETRuntime:Informational:0x19",
            true
        },
        { "gc", "Microsoft-Windows-DotNETRuntime:Verbose:0x1", false },
    };

    [Theory]
    [MemberData(nameof(Profiles))]
    public void ProfilesAskForTheirEvents(string name, string providers, bool rundown)
    {
        var profile = TraceProfiles.Of(name)!;

        Assert.Equal(name, profile.Name);
        Assert.Equal(providers, string.Join(';', profile.Providers.Select(p => p.Name + ":" + p.EventLevel + ":0x" + p.Keywords.ToString("x", System.Globalization.CultureInfo.InvariantCulture))));
        Assert.All(profile.Providers, p => Assert.Null(p.Arguments));
        Assert.Equal(rundown, profile.RequestRundown);
        Assert.Equal(256, TraceProfiles.BufferMB);
    }

    [Theory]
    [InlineData("CPU")]
    [InlineData("")]
    [InlineData("alloc")]
    public void UnknownProfilesHaveNone(string name) => Assert.Null(TraceProfiles.Of(name));

    [Theory]
    [InlineData("cpu")]
    [InlineData("gc")]
    public void NoProfileAsksForExceptions(string name)
    {
        const long exceptionKeyword = 0x8000;
        Assert.All(TraceProfiles.Of(name)!.Providers, p => Assert.Equal(0, p.Keywords & exceptionKeyword));
        Assert.All(TraceProfiles.Of("cpu")!.Providers, p => Assert.Equal(EventLevel.Informational, p.EventLevel));
    }

    public static TheoryData<int, string, long, bool, string?> ParamCases => new()
    {
        { 1, "cpu", 1000, true, null },
        { 1, "gc", TraceMethod.MaxDurationMs, true, null },
        { 0, "cpu", 1000, true, "pid must be positive" },
        { -1, "cpu", 1000, true, "pid must be positive" },
        { 1, "Cpu", 1000, true, "profile must be cpu or gc" },
        { 1, "", 1000, true, "profile must be cpu or gc" },
        { 1, "cpu", 999, true, "durationMs must be 1000 to 300000" },
        { 1, "cpu", TraceMethod.MaxDurationMs + 1, true, "durationMs must be 1000 to 300000" },
        { 1, "cpu", 1000, false, "path must be absolute" },
    };

    [Theory]
    [MemberData(nameof(ParamCases))]
    public void ValidatesParams(int pid, string profile, long durationMs, bool absolute, string? wantError)
    {
        var p = new TraceParams { Pid = pid, Profile = profile, DurationMs = durationMs, Path = absolute ? Path.Combine(_dir, "x.nettrace") : "x.nettrace" };

        if (wantError is null)
        {
            Assert.Equal(p, TraceMethod.Validate(p));
            return;
        }

        var e = Assert.Throws<HelperException>(() => TraceMethod.Validate(p));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.Equal(wantError, e.Message);
    }

    [Theory]
    [InlineData("file")]
    [InlineData("dir")]
    [InlineData("dangling")]
    public void RefusesAnExistingPath(string what)
    {
        var path = Path.Combine(_dir, "x.nettrace");
        switch (what)
        {
            case "file":
                File.WriteAllBytes(path, [1]);
                break;
            case "dir":
                Directory.CreateDirectory(path);
                break;
            default:
                if (OperatingSystem.IsWindows())
                {
                    return; // creating a symlink needs a privilege there
                }

                File.CreateSymbolicLink(path, Path.Combine(_dir, "nowhere"));
                break;
        }

        var e = Assert.Throws<HelperException>(() => TraceMethod.Validate(new TraceParams { Pid = 1, Profile = "cpu", DurationMs = 1000, Path = path }));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.StartsWith("path exists: ", e.Message, StringComparison.Ordinal);

        // The file itself is created only if nothing is there (a name that appeared after the check).
        var created = Assert.Throws<HelperException>(() => TraceFile.Create(path));
        Assert.Equal(ErrorCodes.InvalidRequest, created.Code);
        Assert.False(File.Exists(Path.Combine(_dir, "nowhere")));
        if (what == "file")
        {
            Assert.Equal([1], File.ReadAllBytes(path));
        }
    }

    [Fact]
    public void CreatesTheFileForItsOwnerOnly()
    {
        var path = Path.Combine(_dir, "x.nettrace");

        using (var f = TraceFile.Create(path))
        {
            f.WriteByte(7);
        }

        Assert.Equal([7], File.ReadAllBytes(path));
        if (!OperatingSystem.IsWindows())
        {
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(path));
        }
    }

    [Fact]
    public void CreateInAMissingDirectoryFails()
    {
        var e = Assert.Throws<HelperException>(() => TraceFile.Create(Path.Combine(_dir, "none", "x.nettrace")));

        Assert.Equal(ErrorCodes.HelperFailed, e.Code);
        Assert.StartsWith("creating the trace file failed: ", e.Message, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData(10_000, long.MaxValue, false)]
    [InlineData(10_000, 100, true)]
    [InlineData(0, 1, false)]
    public async Task CopierDrainsTheWholeStream(int size, long limit, bool full)
    {
        var data = Enumerable.Range(0, size).Select(i => (byte)i).ToArray();
        var destination = new MemoryStream();

        var copier = StreamCopier.Start(new MemoryStream(data), destination, limit);
        await copier.Done.WaitAsync(TestContext.Current.CancellationToken);

        // Past the limit it keeps copying: the rest (the rundown at stop) is what makes the file readable.
        Assert.Equal(data, destination.ToArray());
        Assert.Equal(size, copier.Bytes);
        Assert.Equal(full, copier.Full.IsCompleted);
        Assert.Null(copier.WriteError);
        Assert.Throws<ObjectDisposedException>(() => destination.WriteByte(0)); // closed
    }

    [Fact]
    public async Task CopierKeepsDrainingAfterAWriteFailure()
    {
        var source = new MemoryStream(new byte[300_000]);
        var copier = StreamCopier.Start(source, new FailingStream(), long.MaxValue);

        await copier.Done.WaitAsync(TestContext.Current.CancellationToken);

        Assert.Equal(source.Length, source.Position);
        Assert.True(copier.Full.IsCompleted);
        Assert.IsType<IOException>(copier.WriteError);
        Assert.Equal(0, copier.Bytes);
        Assert.Equal("writing the trace failed: No space left on device", TraceCollector.WriteFailed(copier.WriteError!).Message);
    }

    [Fact]
    public async Task CopierEndsOnAReadFailure()
    {
        var source = new MemoryStream([1]);
        source.Dispose();
        var destination = new MemoryStream();

        var copier = StreamCopier.Start(source, destination, long.MaxValue);
        await copier.Done.WaitAsync(TestContext.Current.CancellationToken);

        // The stream is over; the file is closed and nothing failed to write.
        Assert.Null(copier.WriteError);
        Assert.False(copier.Full.IsCompleted);
        Assert.Equal(0, copier.Bytes);
        Assert.Throws<ObjectDisposedException>(() => destination.WriteByte(0));
    }

    [Fact]
    public void StartTimeoutMessageIsUnchanged()
    {
        var e = EventPipeStart.TimedOut(42, "api");

        Assert.Equal(ErrorCodes.DiagnosticsTimeout, e.Code);
        Assert.Equal("pid 42 (api) didn't start a diagnostics session within 5s: it is paused (stopped under a debugger, or suspended) or hung", e.Message);
        Assert.Equal("if a debugger holds it, continue it first ('eyedbg continue'); a stopped .NET runtime can't start a diagnostics session", e.Hint);
    }

    [Fact]
    public void StoppedRespondingSaysWhy()
    {
        var e = TraceCollector.StoppedResponding(42, "api", TraceCollector.StopTimeout);

        Assert.Equal(ErrorCodes.DiagnosticsTimeout, e.Code);
        Assert.StartsWith("pid 42 (api) stopped responding during the trace (paused at a breakpoint, or suspended)", e.Message, StringComparison.Ordinal);
        Assert.Contains("30s", e.Message, StringComparison.Ordinal);
        Assert.Equal("continue it and trace again; don't trace across a breakpoint", e.Hint);
    }

    [Fact]
    public async Task MissingProcessIsRefusedBeforeAnyFile()
    {
        var p = new TraceParams { Pid = int.MaxValue, Profile = "cpu", DurationMs = 1000, Path = Path.Combine(_dir, "x.nettrace") };

        var e = await Assert.ThrowsAsync<HelperException>(() => TraceCollector.RunAsync(p, TestContext.Current.CancellationToken));

        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.False(File.Exists(p.Path));
    }

    [Fact]
    public async Task TracesItself()
    {
        using var load = Load.Spinning();

        var (path, result) = await Load.TraceSelfAsync(_dir, "cpu", TestContext.Current.CancellationToken);

        Assert.Equal(TraceCollector.EndDuration, result.EndReason);
        Assert.True(result.Bytes > 0);
        Assert.Equal(result.Bytes, new FileInfo(path).Length);
        Assert.InRange(result.ElapsedMs, 900, 60_000);
        if (!OperatingSystem.IsWindows())
        {
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, File.GetUnixFileMode(path));
        }
    }

    [Fact]
    public async Task EndsAtTheSizeLimit()
    {
        using var load = Load.Spinning();
        var p = new TraceParams { Pid = Environment.ProcessId, Profile = "cpu", DurationMs = TraceMethod.MaxDurationMs, Path = Path.Combine(_dir, "x.nettrace") };

        var result = await TraceCollector.RunAsync(p, maxBytes: 1, TraceCollector.StopTimeout, TestContext.Current.CancellationToken);

        Assert.Equal(TraceCollector.EndSize, result.EndReason);
        Assert.Equal(result.Bytes, new FileInfo(p.Path).Length);
    }

    [Fact]
    public async Task CancelingStopsTheSession()
    {
        using var cancel = CancellationTokenSource.CreateLinkedTokenSource(TestContext.Current.CancellationToken);
        var p = new TraceParams { Pid = Environment.ProcessId, Profile = "gc", DurationMs = TraceMethod.MaxDurationMs, Path = Path.Combine(_dir, "x.nettrace") };

        var run = TraceCollector.RunAsync(p, cancel.Token);
        await cancel.CancelAsync();

        await Assert.ThrowsAnyAsync<OperationCanceledException>(() => run);

        // The file is closed: eyedbg removes it.
        File.Delete(p.Path);
    }

    private sealed class FailingStream : Stream
    {
        public override bool CanRead => false;

        public override bool CanSeek => false;

        public override bool CanWrite => true;

        public override long Length => throw new NotSupportedException();

        public override long Position
        {
            get => throw new NotSupportedException();
            set => throw new NotSupportedException();
        }

        public override void Flush()
        {
        }

        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException();

        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

        public override void SetLength(long value) => throw new NotSupportedException();

        public override void Write(byte[] buffer, int offset, int count) => throw new IOException("No space left on device");
    }
}
