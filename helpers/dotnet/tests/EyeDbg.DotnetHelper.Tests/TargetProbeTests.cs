// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public class TargetProbeTests
{
    public static TheoryData<string?, string[]?, string> Cases => new()
    {
        { "dotnet", null, ErrorCodes.DiagnosticsDisabled },
        { "DOTNET", [], ErrorCodes.DiagnosticsDisabled },
        { "myapp", ["/usr/lib/libc.so.6", "/usr/share/dotnet/shared/Microsoft.NETCore.App/10.0.0/libcoreclr.so"], ErrorCodes.DiagnosticsDisabled },
        { "myapp", [@"C:\Program Files\dotnet\shared\Microsoft.NETCore.App\10.0.0\coreclr.dll"], ErrorCodes.DiagnosticsDisabled },
        { "myapp", ["CoreCLR.DLL"], ErrorCodes.DiagnosticsDisabled },
        { "myapp", ["/x/libcoreclr.dylib"], ErrorCodes.DiagnosticsDisabled },
        { "myapp", ["/x/libcoreclrtraceptprovider.so", "/x/notcoreclr.dll"], ErrorCodes.NotDotnet },
        { "sleep", ["/usr/lib/libc.so.6"], ErrorCodes.NotDotnet },
        { "explorer", null, ErrorCodes.NotDotnet }, // module list unreadable
        { null, null, ErrorCodes.NotDotnet },
        { "dotnet-counters", [], ErrorCodes.NotDotnet },
    };

    [Theory]
    [MemberData(nameof(Cases))]
    public void ClassifiesAMissingEndpoint(string? name, string[]? modules, string want)
    {
        var e = TargetProbe.NoEndpoint(42, new TargetFacts(name, modules));

        Assert.Equal(want, e.Code);
        Assert.StartsWith(name is null ? "pid 42 " : "pid 42 (" + name + ") ", e.Message, StringComparison.Ordinal);
        Assert.False(string.IsNullOrEmpty(e.Hint));
    }

    [Fact]
    public void FindsItself()
    {
        using var self = TargetProbe.Find(Environment.ProcessId);
        var facts = TargetProbe.Facts(self);

        Assert.False(string.IsNullOrEmpty(facts.Name));
    }

    [Fact]
    public void MissingProcessIsInvalidRequest()
    {
        var e = Assert.Throws<HelperException>(() => TargetProbe.Find(int.MaxValue));

        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.Equal("no process 2147483647", e.Message);
    }

    public static TheoryData<string[], bool, bool> Pipes => new()
    {
        { [], false, false },
        { ["dotnet-diagnostic-42"], true, false },
        { ["DOTNET-DIAGNOSTIC-42", "dotnet-diagnostic-420", "dotnet-diagnostic-4"], true, false },
        { ["dotnet-diagnostic-42", "dotnet-diagnostic-dsrouter-42"], true, true },
        { ["Dotnet-Diagnostic-DSRouter-42"], false, true }, // the router alone is used too
        { ["dotnet-diagnostic-dsrouter-420", "dotnet-diagnostic-dsrouter-42-1-socket"], false, false },
    };

    [Theory]
    [MemberData(nameof(Pipes))]
    public void ClassifiesWindowsPipes(string[] names, bool endpoint, bool router) =>
        Assert.Equal(new WindowsPipes(endpoint, router), WindowsPipes.Of(names, 42));

    [Fact]
    public void RouterPipeIsRefused()
    {
        var e = TargetProbe.RouterPipe(42, "api");

        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.StartsWith("pid 42 (api): refused, a pipe named dotnet-diagnostic-dsrouter-42 exists", e.Message, StringComparison.Ordinal);
        Assert.Contains("any user can create", e.Hint, StringComparison.Ordinal);
    }
}
