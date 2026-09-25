// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Dumps;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Tests;

public sealed class DumpTests : IDisposable
{
    private readonly string _dir = Directory.CreateTempSubdirectory("eyedbg-dump-tests-").FullName;

    public void Dispose() => Directory.Delete(_dir, recursive: true);

    public static TheoryData<int, string, bool, long, string?> ParamCases => new()
    {
        { 1, "heap", true, 1000, null },
        { 1, "mini", true, DumpMethod.MaxTimeoutMs, null },
        { 1, "triage", true, 60_000, null },
        { 1, "full", true, 60_000, null },
        { 0, "heap", true, 60_000, "pid must be positive" },
        { -1, "heap", true, 60_000, "pid must be positive" },
        { 1, "Heap", true, 60_000, "type must be heap, mini, triage or full" },
        { 1, "", true, 60_000, "type must be heap, mini, triage or full" },
        { 1, "heap", false, 60_000, "path must be absolute" },
        { 1, "heap", true, 999, "timeoutMs must be 1000 to 3600000" },
        { 1, "heap", true, DumpMethod.MaxTimeoutMs + 1, "timeoutMs must be 1000 to 3600000" },
    };

    [Theory]
    [MemberData(nameof(ParamCases))]
    public void ValidatesParams(int pid, string type, bool absolute, long timeoutMs, string? wantError)
    {
        var p = new DumpParams { Pid = pid, Type = type, Path = absolute ? Path.Combine(_dir, "x.dmp") : "x.dmp", TimeoutMs = timeoutMs };

        if (wantError is null)
        {
            Assert.Equal(p, DumpMethod.Validate(p));
            return;
        }

        var e = Assert.Throws<HelperException>(() => DumpMethod.Validate(p));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.Equal(wantError, e.Message);
    }

    [Theory]
    [InlineData("file")]
    [InlineData("dir")]
    [InlineData("dangling")]
    public void RefusesAnExistingPath(string what)
    {
        var path = Path.Combine(_dir, "x.dmp");
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

        var e = Assert.Throws<HelperException>(() => DumpMethod.Validate(new DumpParams { Pid = 1, Type = "heap", Path = path, TimeoutMs = 1000 }));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.StartsWith("path exists: ", e.Message, StringComparison.Ordinal);
        Assert.False(File.Exists(Path.Combine(_dir, "nowhere")));
    }

    public static TheoryData<Exception, bool, string?> Failures => new()
    {
        { new OperationCanceledException(), false, ErrorCodes.DiagnosticsTimeout },
        { new TaskCanceledException(), false, ErrorCodes.DiagnosticsTimeout },
        { new TimeoutException(), false, ErrorCodes.DiagnosticsTimeout },
        { new OperationCanceledException(), true, null }, // eyedbg stopped waiting
        { new ServerNotAvailableException("no endpoint"), false, ErrorCodes.NotDotnet },
        { new ServerErrorException("Writing dump failed (HRESULT: 0x80004005)"), false, ErrorCodes.DumpFailed },
        { new InvalidOperationException("x"), false, null }, // HELPER_FAILED at the server
    };

    [Theory]
    [MemberData(nameof(Failures))]
    public void MapsFailures(Exception e, bool canceledByCaller, string? wantCode)
    {
        var p = new DumpParams { Pid = 42, Type = "heap", Path = "/d/x.dmp", TimeoutMs = 2500 };

        var got = DumpWriter.Map(e, p, "api", canceledByCaller);

        Assert.Equal(wantCode, got?.Code);
        if (got is not null)
        {
            Assert.StartsWith("pid 42 (api) ", got.Message, StringComparison.Ordinal);
            Assert.False(string.IsNullOrEmpty(got.Hint));
        }

        if (wantCode == ErrorCodes.DiagnosticsTimeout)
        {
            Assert.Contains("within 2.5s", got!.Message, StringComparison.Ordinal);
        }

        if (wantCode == ErrorCodes.DumpFailed)
        {
            Assert.EndsWith("Writing dump failed (HRESULT: 0x80004005)", got!.Message, StringComparison.Ordinal);
        }
    }

    [Fact]
    public void NotArrivedNamesThePath()
    {
        var e = DumpWriter.NotArrived("/d/x.dmp");

        Assert.Equal(ErrorCodes.DumpFailed, e.Code);
        Assert.Equal("the runtime reported a dump but none is at /d/x.dmp", e.Message);
    }

    [Fact]
    public async Task MissingProcessIsRefused()
    {
        // No such process: INVALID_REQUEST before any dump (the not-.NET case is a real process: e2e).
        var p = new DumpParams { Pid = int.MaxValue, Type = "mini", Path = Path.Combine(_dir, "x.dmp"), TimeoutMs = 1000 };

        var e = await Assert.ThrowsAsync<HelperException>(() => DumpWriter.WriteAsync(p, TestContext.Current.CancellationToken));

        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.False(File.Exists(p.Path));
    }
}
