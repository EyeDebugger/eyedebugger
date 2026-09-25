// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics;
using System.Globalization;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Dumps;

/// <summary>
/// Asks a process's runtime to write a dump of itself (createdump) at a path eyedbg chose: a
/// fresh name in its private dumps directory (docs/adr/0016, P2-M7). The runtime suspends the
/// process while it writes.
/// </summary>
internal static class DumpWriter
{
    /// <summary>The dump types of the protocol and the runtime's names for them.</summary>
    public static readonly IReadOnlyDictionary<string, DumpType> Types = new Dictionary<string, DumpType>(StringComparer.Ordinal)
    {
        ["heap"] = DumpType.WithHeap,
        ["mini"] = DumpType.Normal,
        ["triage"] = DumpType.Triage,
        ["full"] = DumpType.Full,
    };

    /// <summary>Writes the dump; returns its size and how long it took.</summary>
    public static async Task<DumpResult> WriteAsync(DumpParams p, CancellationToken cancellationToken)
    {
        using var process = TargetProbe.Find(p.Pid);
        TargetProbe.CheckEndpoint(p.Pid, process);

        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(TimeSpan.FromMilliseconds(p.TimeoutMs));
        var clock = Stopwatch.StartNew();
        try
        {
            // Permitted DiagnosticsClient calls only (docs/adr/0016): WriteDumpAsync joined
            // GetPublishedProcesses and StartEventPipeSession[Async] in its P2-M7 addendum. Never
            // the logging or crash-report flags: they write into the target's own output.
            await new DiagnosticsClient(p.Pid)
                .WriteDumpAsync(Types[p.Type], p.Path, WriteDumpFlags.None, deadline.Token)
                .ConfigureAwait(false);
        }
        catch (Exception e) when (Map(e, p, TargetProbe.NameOf(process), cancellationToken.IsCancellationRequested) is { } mapped)
        {
            throw mapped;
        }

        var elapsed = clock.ElapsedMilliseconds;
        var file = new FileInfo(p.Path);
        if (!file.Exists)
        {
            throw NotArrived(p.Path);
        }

        return new DumpResult(file.Length, elapsed);
    }

    /// <summary>
    /// The error for what WriteDumpAsync threw, or null to let it through (the caller canceled:
    /// eyedbg stopped waiting; or an unexpected exception, which becomes HELPER_FAILED).
    /// </summary>
    internal static HelperException? Map(Exception e, DumpParams p, string? name, bool canceledByCaller) => e switch
    {
        OperationCanceledException when canceledByCaller => null,
        OperationCanceledException or TimeoutException => TimedOut(p, name),
        ServerNotAvailableException => TargetProbe.NoEndpoint(p.Pid, new TargetFacts(name, null)),

        // createdump failed: the runtime's message names the path and an HRESULT, no process data.
        ServerErrorException => new HelperException(
            ErrorCodes.DumpFailed,
            TargetProbe.Describe(p.Pid, name) + " couldn't write the dump: " + Bounded(e.Message),
            "check the free space where dumps go; a full dump needs about the process's memory size"),
        _ => null,
    };

    private static HelperException TimedOut(DumpParams p, string? name) => new(
        ErrorCodes.DiagnosticsTimeout,
        TargetProbe.Describe(p.Pid, name) + " didn't finish writing the dump within " + Seconds(p.TimeoutMs) + ": it is paused (suspended) or hung, or the dump is very large",
        "raise --timeout (full dumps take long)");

    /// <summary>The runtime answered but no file is there: it wrote into another filesystem view.</summary>
    internal static HelperException NotArrived(string path) => new(
        ErrorCodes.DumpFailed,
        "the runtime reported a dump but none is at " + path,
        "the process runs in another filesystem namespace (a container?); take the dump inside it");

    private static string Bounded(string s) => s.Length <= 300 ? s : s[..300];

    private static string Seconds(long ms) => (ms / 1000.0).ToString("0.###", CultureInfo.InvariantCulture) + "s";
}
