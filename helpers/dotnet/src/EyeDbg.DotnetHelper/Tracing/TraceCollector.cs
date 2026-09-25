// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>
/// Records an EventPipe trace of a process into a file (docs/adr/0016, P2-M8): bounded start,
/// the stream copied to the file on its own thread, then a bounded stop that lets the runtime
/// write the rundown (the method and module names), without which the file can't be read.
/// </summary>
internal static class TraceCollector
{
    /// <summary>A trace ends early at this size (plus what the rundown adds at stop).</summary>
    public const long MaxBytes = 512L * 1024 * 1024;

    /// <summary>Bounds stopping the session, the rundown included.</summary>
    public static readonly TimeSpan StopTimeout = TimeSpan.FromSeconds(30);

    /// <summary>Bounds stopping a session whose file won't be used (canceled, or writing it failed).</summary>
    public static readonly TimeSpan AbandonTimeout = TimeSpan.FromSeconds(3);

    /// <summary>After the stream is closed, how long the copying thread gets to notice.</summary>
    private static readonly TimeSpan _copierExitTimeout = TimeSpan.FromSeconds(1);

    public const string EndDuration = "duration";
    public const string EndExited = "exited";
    public const string EndSize = "size";

    public static Task<TraceResult> RunAsync(TraceParams p, CancellationToken cancellationToken) =>
        RunAsync(p, MaxBytes, StopTimeout, cancellationToken);

    /// <summary>
    /// Records p.DurationMs of the profile into p.Path. Canceled: stops the session (bounded) and
    /// throws OperationCanceledException; the file is then incomplete, and eyedbg removes it.
    /// </summary>
    internal static async Task<TraceResult> RunAsync(TraceParams p, long maxBytes, TimeSpan stopTimeout, CancellationToken cancellationToken)
    {
        var profile = TraceProfiles.Of(p.Profile) ?? throw new HelperException(ErrorCodes.InvalidRequest, "profile must be cpu or gc");
        using var process = TargetProbe.Find(p.Pid);
        TargetProbe.CheckEndpoint(p.Pid, process);

        // The file first: when it can't be created, the target never sees a session.
        var file = TraceFile.Create(p.Path);
        EventPipeSession session;
        try
        {
            session = await EventPipeStart.StartAsync(p.Pid, process, profile.Providers, profile.RequestRundown, TraceProfiles.BufferMB, cancellationToken)
                .ConfigureAwait(false);
        }
        catch
        {
            await file.DisposeAsync().ConfigureAwait(false);
            throw;
        }

        using (session)
        {
            var clock = Stopwatch.StartNew();
            var copier = StreamCopier.Start(session.EventStream, file, maxBytes);
            using (var wait = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken))
            {
                var duration = Task.Delay(TimeSpan.FromMilliseconds(p.DurationMs), wait.Token);
                await Task.WhenAny(copier.Done, copier.Full, duration).ConfigureAwait(false);
                await wait.CancelAsync().ConfigureAwait(false);
            }

            if (cancellationToken.IsCancellationRequested)
            {
                await StopAsync(session, copier, AbandonTimeout).ConfigureAwait(false);
                cancellationToken.ThrowIfCancellationRequested();
            }

            string endReason;
            if (copier.Done.IsCompleted)
            {
                // The stream ended before it was stopped: the target exited (its runtime ends the
                // stream properly on a normal exit; a killed one leaves a file that won't parse).
                endReason = EndExited;
            }
            else if (copier.WriteError is not null)
            {
                await StopAsync(session, copier, AbandonTimeout).ConfigureAwait(false);
                throw WriteFailed(copier.WriteError);
            }
            else
            {
                endReason = copier.Bytes >= maxBytes ? EndSize : EndDuration;
                var stop = await StopAsync(session, copier, stopTimeout).ConfigureAwait(false);
                if (stop == Stop.Closed)
                {
                    throw StoppedResponding(p.Pid, TargetProbe.NameOf(process), stopTimeout);
                }

                if (stop == Stop.Exited)
                {
                    // It exited while being stopped; its runtime ended the stream itself.
                    endReason = EndExited;
                }
            }

            if (copier.WriteError is { } writeError)
            {
                throw WriteFailed(writeError);
            }

            return new TraceResult(copier.Bytes, clock.ElapsedMilliseconds, endReason);
        }
    }

    /// <summary>How stopping a session went.</summary>
    private enum Stop
    {
        /// <summary>The stream ended after the stop: the rundown is in the file.</summary>
        Ended,

        /// <summary>The target was gone when asked to stop; the stream ended.</summary>
        Exited,

        /// <summary>The stream didn't end in time and was closed under the copier: the file is incomplete.</summary>
        Closed,
    }

    /// <summary>
    /// Stops the session and waits for the stream's end within timeout; a paused runtime may
    /// never answer, so the stream is then closed under the copier.
    /// </summary>
    private static async Task<Stop> StopAsync(EventPipeSession session, StreamCopier copier, TimeSpan timeout)
    {
        var noServer = false;
        using (var stop = new CancellationTokenSource(timeout))
        {
            try
            {
                await session.StopAsync(stop.Token).ConfigureAwait(false);
            }
            catch (ServerNotAvailableException)
            {
                noServer = true;
            }
            catch (Exception e) when (e is OperationCanceledException or IOException or TimeoutException)
            {
            }

            try
            {
                await copier.Done.WaitAsync(stop.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
            }
        }

        if (copier.Done.IsCompleted)
        {
            return noServer ? Stop.Exited : Stop.Ended;
        }

        session.Dispose();
        try
        {
            await copier.Done.WaitAsync(_copierExitTimeout).ConfigureAwait(false);
        }
        catch (TimeoutException)
        {
            // The copying thread is a background thread; the process exits after answering.
        }

        return Stop.Closed;
    }

    internal static HelperException StoppedResponding(int pid, string? name, TimeSpan timeout) => new(
        ErrorCodes.DiagnosticsTimeout,
        TargetProbe.Describe(pid, name) + " stopped responding during the trace (paused at a breakpoint, or suspended): the trace didn't end within " + EventPipeStart.Seconds((long)timeout.TotalMilliseconds) + " and can't be read",
        "continue it and trace again; don't trace across a breakpoint");

    internal static HelperException WriteFailed(Exception e) => new(
        ErrorCodes.HelperFailed,
        "writing the trace failed: " + e.Message,
        "check the free space where traces go");
}
