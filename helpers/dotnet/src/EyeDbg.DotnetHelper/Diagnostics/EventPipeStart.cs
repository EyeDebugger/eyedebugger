// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics;
using System.Globalization;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Diagnostics;

/// <summary>
/// Starts an EventPipe session in a process within <see cref="Timeout"/>, mapping the ways it
/// fails to eyedbg's codes (counters and traces).
/// </summary>
internal static class EventPipeStart
{
    /// <summary>A runtime that can't run managed code (stopped at a breakpoint) never answers the start.</summary>
    public static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);

    public static async Task<EventPipeSession> StartAsync(
        int pid,
        Process process,
        IEnumerable<EventPipeProvider> providers,
        bool requestRundown,
        int circularBufferMB,
        CancellationToken cancellationToken)
    {
        using var start = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        start.CancelAfter(Timeout);
        try
        {
            // Permitted DiagnosticsClient calls only (docs/adr/0016): GetPublishedProcesses,
            // StartEventPipeSession[Async] and, for dumps, WriteDumpAsync.
            return await new DiagnosticsClient(pid)
                .StartEventPipeSessionAsync(providers, requestRundown, circularBufferMB, start.Token)
                .ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw TimedOut(pid, TargetProbe.NameOf(process));
        }
        catch (TimeoutException)
        {
            throw TimedOut(pid, TargetProbe.NameOf(process));
        }
        catch (ServerNotAvailableException)
        {
            throw TargetProbe.NoEndpoint(pid, TargetProbe.Facts(process));
        }
    }

    internal static HelperException TimedOut(int pid, string? name) => new(
        ErrorCodes.DiagnosticsTimeout,
        TargetProbe.Describe(pid, name) + " didn't start a diagnostics session within " + Seconds((long)Timeout.TotalMilliseconds) + ": it is paused (stopped under a debugger, or suspended) or hung",
        "if a debugger holds it, continue it first ('eyedbg continue'); a stopped .NET runtime can't start a diagnostics session");

    /// <summary>Milliseconds as seconds for a message: "2.5s".</summary>
    public static string Seconds(long ms) => (ms / 1000.0).ToString("0.###", CultureInfo.InvariantCulture) + "s";
}
