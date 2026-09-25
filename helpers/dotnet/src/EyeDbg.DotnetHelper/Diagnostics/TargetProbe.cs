// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics;
using System.Globalization;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Diagnostics;

/// <summary>What the helper can tell about a process without its diagnostics endpoint.</summary>
/// <param name="Name">The process name, if readable.</param>
/// <param name="Modules">Loaded module file names, if readable (not on macOS).</param>
internal sealed record TargetFacts(string? Name, IReadOnlyList<string>? Modules);

/// <summary>Finds a target process and explains a missing diagnostics endpoint.</summary>
internal static class TargetProbe
{
    private static readonly string[] _coreclrModules = ["libcoreclr.so", "libcoreclr.dylib", "coreclr.dll"];

    /// <summary>The process, or INVALID_REQUEST when there is none.</summary>
    public static Process Find(int pid)
    {
        try
        {
            return Process.GetProcessById(pid);
        }
        catch (ArgumentException)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "no process " + pid.ToString(CultureInfo.InvariantCulture));
        }
        catch (InvalidOperationException)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "no process " + pid.ToString(CultureInfo.InvariantCulture));
        }
    }

    /// <summary>The process's name and modules; what can't be read is null.</summary>
    public static TargetFacts Facts(Process process)
    {
        string? name = null;
        List<string>? modules;
        try
        {
            name = process.ProcessName;
        }
        catch (Exception e) when (e is InvalidOperationException or NotSupportedException or System.ComponentModel.Win32Exception)
        {
        }

        try
        {
            modules = [];
            foreach (ProcessModule module in process.Modules)
            {
                if (module.ModuleName is { } m)
                {
                    modules.Add(m);
                }
            }
        }
        catch (Exception e) when (e is InvalidOperationException or NotSupportedException or System.ComponentModel.Win32Exception)
        {
            modules = null;
        }

        return new TargetFacts(name, modules);
    }

    /// <summary>The process's name, or null when it can't be read.</summary>
    public static string? NameOf(Process process)
    {
        try
        {
            return process.ProcessName;
        }
        catch (Exception e) when (e is InvalidOperationException or NotSupportedException or System.ComponentModel.Win32Exception)
        {
            return null;
        }
    }

    /// <summary>
    /// Refuses a process whose diagnostics endpoint can't be used, before connecting to it. Only
    /// Windows can tell in advance: its pipes are listed (a missing one would make NETCore.Client
    /// wait until its timeout), and a dotnet-dsrouter pipe is refused (see <see cref="RouterPipe"/>).
    /// Elsewhere a missing endpoint shows when connecting (ServerNotAvailableException).
    /// </summary>
    public static void CheckEndpoint(int pid, Process process)
    {
        if (!OperatingSystem.IsWindows())
        {
            return;
        }

        // The router pipe first: NETCore.Client would use it even without the real one.
        var pipes = FindWindowsPipes(pid);
        if (pipes.Router)
        {
            throw RouterPipe(pid, NameOf(process));
        }

        if (!pipes.Endpoint)
        {
            throw NoEndpoint(pid, Facts(process));
        }
    }

    /// <summary>"pid N (name)", or "pid N" without a name.</summary>
    public static string Describe(int pid, string? name) =>
        "pid " + pid.ToString(CultureInfo.InvariantCulture) + (string.IsNullOrEmpty(name) ? "" : " (" + name + ")");

    /// <summary>
    /// Whether a process shows signs of .NET without its endpoint: named dotnet (the host), or a
    /// coreclr module loaded (readable on Linux and Windows).
    /// </summary>
    public static bool LooksLikeDotnet(TargetFacts facts) =>
        string.Equals(facts.Name, "dotnet", StringComparison.OrdinalIgnoreCase)
        || (facts.Modules?.Any(m => _coreclrModules.Contains(FileName(m), StringComparer.OrdinalIgnoreCase)) ?? false);

    /// <summary>A path's last element, for either OS's separators (the facts are OS-independent).</summary>
    private static string FileName(string path) => path[(path.LastIndexOfAny(['/', '\\']) + 1)..];

    /// <summary>The error for a process with no diagnostics endpoint (docs/adr/0016 D9).</summary>
    public static HelperException NoEndpoint(int pid, TargetFacts facts)
    {
        var who = Describe(pid, facts.Name);
        return LooksLikeDotnet(facts)
            ? new HelperException(
                ErrorCodes.DiagnosticsDisabled,
                who + " is a .NET process with no diagnostics endpoint",
                "it was started with DOTNET_EnableDiagnostics=0 (or DOTNET_EnableDiagnostics_IPC=0), or it uses another TMPDIR than you (macOS)")
            : new HelperException(
                ErrorCodes.NotDotnet,
                who + " has no .NET diagnostics endpoint",
                "not a .NET (Core 3.0 or later) process, or one started with DOTNET_EnableDiagnostics=0 or another TMPDIR; 'eyedbg dotnet ps' lists the ones you can inspect");
    }

    /// <summary>
    /// Windows: the process's diagnostics pipes, listed the way NETCore.Client lists them (its
    /// connect would otherwise wait for a missing pipe until its timeout). When the list can't
    /// be read: an endpoint and no router, and the bounded session start decides.
    /// </summary>
    public static WindowsPipes FindWindowsPipes(int pid)
    {
        try
        {
            return WindowsPipes.Of(Directory.GetFiles(@"\\.\pipe\").Select(Path.GetFileName), pid);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException or ArgumentException)
        {
            return new WindowsPipes(Endpoint: true, Router: false);
        }
    }

    /// <summary>
    /// The error for a process with a dotnet-dsrouter pipe (docs/adr/0016): NETCore.Client
    /// connects to that pipe instead of the process's own, and any user can create one.
    /// </summary>
    public static HelperException RouterPipe(int pid, string? name) => new(
        ErrorCodes.InvalidRequest,
        Describe(pid, name) + ": refused, a pipe named " + WindowsPipes.RouterName(pid) + " exists, and the .NET diagnostics client would connect to it instead of the process's own endpoint",
        "any user can create that pipe: unless you run dotnet-dsrouter as this pid, another user may be posing as its endpoint (see 'eyedbg help dotnet')");
}

/// <summary>Which of a process's diagnostics pipes exist (Windows).</summary>
/// <param name="Endpoint">dotnet-diagnostic-PID: the runtime's own.</param>
/// <param name="Router">dotnet-diagnostic-dsrouter-PID, which NETCore.Client prefers.</param>
internal sealed record WindowsPipes(bool Endpoint, bool Router)
{
    public static string EndpointName(int pid) => "dotnet-diagnostic-" + pid.ToString(CultureInfo.InvariantCulture);

    public static string RouterName(int pid) => "dotnet-diagnostic-dsrouter-" + pid.ToString(CultureInfo.InvariantCulture);

    /// <summary>Classifies pipe names, compared as Windows does: ignoring case.</summary>
    public static WindowsPipes Of(IEnumerable<string?> names, int pid)
    {
        var endpoint = EndpointName(pid);
        var router = RouterName(pid);
        bool hasEndpoint = false, hasRouter = false;
        foreach (var n in names)
        {
            hasEndpoint |= string.Equals(n, endpoint, StringComparison.OrdinalIgnoreCase);
            hasRouter |= string.Equals(n, router, StringComparison.OrdinalIgnoreCase);
        }

        return new WindowsPipes(hasEndpoint, hasRouter);
    }
}
