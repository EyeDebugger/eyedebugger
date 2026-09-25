// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics;
using System.Diagnostics.Tracing;
using System.Globalization;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;
using Microsoft.Diagnostics.Tracing;

namespace EyeDbg.DotnetHelper.Counters;

/// <summary>
/// Collects System.Runtime EventCounters from a process over an EventPipe session: bounded
/// start, samples streamed as they arrive, bounded stop.
/// </summary>
internal static class CounterSession
{
    /// <summary>Bounds stopping the session and reading what the runtime still sends.</summary>
    public static readonly TimeSpan StopTimeout = TimeSpan.FromSeconds(3);

    /// <summary>After the stream is closed, how long the reading thread gets to notice.</summary>
    private static readonly TimeSpan _pumpExitTimeout = TimeSpan.FromSeconds(1);

    public const string EndDuration = "duration";
    public const string EndExited = "exited";

    /// <summary>
    /// Runs the session for durationMs (0: until canceled), sending each sample through emit.
    /// Canceled: stops the session (bounded) and throws OperationCanceledException.
    /// </summary>
    public static async Task<CountersResult> RunAsync(CountersParams p, Action<CounterSample> emit, CancellationToken cancellationToken)
    {
        using var process = TargetProbe.Find(p.Pid);
        TargetProbe.CheckEndpoint(p.Pid, process);

        var session = await StartAsync(p, process, cancellationToken).ConfigureAwait(false);
        using (session)
        {
            var clock = Stopwatch.StartNew();
            var batcher = new SampleBatcher(() => clock.ElapsedMilliseconds, emit);
            var stream = new EndTrackingStream(session.EventStream);
            var pump = EventPump.Start(stream, e => OnCounters(e, batcher));

            bool exited;
            using (var wait = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken))
            {
                var duration = Task.Delay(p.DurationMs > 0 ? TimeSpan.FromMilliseconds(p.DurationMs) : Timeout.InfiniteTimeSpan, wait.Token);
                var first = await Task.WhenAny(pump.Done, duration, batcher.Failed).ConfigureAwait(false);
                await wait.CancelAsync().ConfigureAwait(false);
                exited = first == pump.Done;
            }

            if (!exited)
            {
                await StopAsync(session, pump).ConfigureAwait(false);
            }

            // Canceled: eyedbg has stopped reading; nothing more is sent.
            batcher.Close(flush: !cancellationToken.IsCancellationRequested);

            if (batcher.EmitError is { } emitError)
            {
                throw new IOException("sending a sample failed", emitError);
            }

            cancellationToken.ThrowIfCancellationRequested();

            // The stream ended by itself: the target exited (a cut-off stream makes the parser
            // fail too). A parser failure on a stream that hadn't ended is a real error.
            if (exited && pump.Done.Result is { } readError && !stream.Ended)
            {
                throw Server.Failure(readError);
            }

            if (batcher.Samples == 0 && !exited)
            {
                throw new HelperException(
                    ErrorCodes.DiagnosticsTimeout,
                    "no samples from " + TargetProbe.Describe(p.Pid, TargetProbe.NameOf(process)) + " in " + Seconds(p.DurationMs) + ": it is paused (stopped under a debugger, or suspended) or hung",
                    "continue it if a debugger holds it, then try again");
            }

            return new CountersResult(batcher.Samples, exited ? EndExited : EndDuration);
        }
    }

    private static Task<EventPipeSession> StartAsync(CountersParams p, Process process, CancellationToken cancellationToken)
    {
        var providers = new[]
        {
            new EventPipeProvider(
                CounterPayload.RuntimeProvider,
                EventLevel.Informational,
                0,
                new Dictionary<string, string>(StringComparer.Ordinal)
                {
                    ["EventCounterIntervalSec"] = p.IntervalSec.ToString(CultureInfo.InvariantCulture),
                }),
        };

        return EventPipeStart.StartAsync(p.Pid, process, providers, requestRundown: false, circularBufferMB: 4, cancellationToken);
    }

    /// <summary>
    /// Stops the session and reads what the runtime still sends, within <see cref="StopTimeout"/>;
    /// a paused runtime may never answer, so the stream is then closed under the reader.
    /// </summary>
    private static async Task StopAsync(EventPipeSession session, EventPump pump)
    {
        using (var stop = new CancellationTokenSource(StopTimeout))
        {
            try
            {
                await session.StopAsync(stop.Token).ConfigureAwait(false);
                await pump.Done.WaitAsync(stop.Token).ConfigureAwait(false);
            }
            catch (Exception e) when (e is OperationCanceledException or ServerNotAvailableException or IOException or TimeoutException)
            {
            }
        }

        session.Dispose();
        try
        {
            await pump.Done.WaitAsync(_pumpExitTimeout).ConfigureAwait(false);
        }
        catch (TimeoutException)
        {
            // The reading thread is a background thread and drops what it still reads.
        }
    }

    private static void OnCounters(TraceEvent e, SampleBatcher batcher)
    {
        if (e.ProviderName != CounterPayload.RuntimeProvider)
        {
            return;
        }

        if (e.PayloadNames.Length == 0)
        {
            return;
        }

        if (CounterPayload.From(e.ProviderName, CounterPayload.Fields(e.PayloadValue(0))) is { } value)
        {
            batcher.Add(value);
        }
    }

    private static string Seconds(long ms) => EventPipeStart.Seconds(ms);
}
