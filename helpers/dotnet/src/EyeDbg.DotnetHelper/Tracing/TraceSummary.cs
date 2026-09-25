// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Diagnostics;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Tracing;
using Microsoft.Diagnostics.Tracing.Analysis;
using Microsoft.Diagnostics.Tracing.Etlx;
using Microsoft.Diagnostics.Tracing.EventPipe;
using Microsoft.Diagnostics.Tracing.Parsers.Clr;
using TraceLog = Microsoft.Diagnostics.Tracing.Etlx.TraceLog;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>
/// Summarizes a .nettrace file (docs/adr/0016, P2-M8). Pass 1 reads it once for GC events,
/// allocation ticks and the number of CPU samples; pass 2, only with samples, converts it to
/// TraceLog's indexed form (ETLX) at the scratch path to resolve the samples' stacks. Names come
/// from the trace itself (the rundown): no symbol server, no symbol lookup, and no file a trace
/// names is ever opened. Any .nettrace is untrusted input.
/// </summary>
internal static class TraceSummary
{
    /// <summary>How often (in events) the deadline is checked.</summary>
    private const int CheckEvery = 4096;

    public static TraceSummaryResult Run(TraceSummaryParams p, CancellationToken cancellationToken)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(TimeSpan.FromMilliseconds(p.TimeoutMs));
        try
        {
            return Summarize(p, deadline.Token);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw new HelperException(
                ErrorCodes.DiagnosticsTimeout,
                "the summary didn't finish within " + EventPipeStart.Seconds(p.TimeoutMs) + " (a large trace?)",
                "raise --timeout");
        }
    }

    private static TraceSummaryResult Summarize(TraceSummaryParams p, CancellationToken token)
    {
        if (new FileInfo(p.Path).Length == 0)
        {
            // Also what a FIFO or a device reports: never opened, it could block.
            throw Unsupported("the trace is empty");
        }

        var gc = new GcAggregator();
        var (samples, durationMs, eventsLost) = ReadEvents(p.Path, gc, token);
        token.ThrowIfCancellationRequested();

        StackAggregator? stacks = null;
        if (samples > 0)
        {
            stacks = new StackAggregator();
            ReadStacks(p.Path, p.Scratch, stacks, token);
        }

        // A readable trace with nothing in it (an idle process traced for gc) is an answer, not
        // an error: the result then has neither cpu, gc nor allocations (M8-Q13).
        var budget = new ResultBudget();
        var cpu = stacks is { Samples: > 0 } ? stacks.Result(p.Top, budget) : null;
        return new TraceSummaryResult(durationMs, eventsLost, cpu, gc.Gc(), gc.Allocations(p.Top, budget));
    }

    /// <summary>Pass 1: GCs and allocation ticks into gc; returns the number of samples, the session's duration and events lost.</summary>
    private static (long Samples, long DurationMs, long EventsLost) ReadEvents(string path, GcAggregator gc, CancellationToken token)
    {
        try
        {
            using var source = new EventPipeEventSource(path);
            source.NeedLoadedDotNetRuntimes();
            source.AddCallbackOnProcessStart(process => process.AddCallbackOnDotNetRuntimeLoad(runtime =>
                runtime.GCEnd += (_, g) => gc.AddGc(g.Generation, g.Type == GCType.BackgroundGC, g.Reason.ToString(), g.PauseDurationMSec)));
            source.Clr.GCAllocationTick += a => gc.AddTick(a.TypeName, a.AllocationAmount64);
            long samples = 0;
            new SampleProfilerTraceEventParser(source).ThreadSample += _ => samples++;
            long events = 0;
            source.AllEvents += _ =>
            {
                if (++events % CheckEvery == 0 && token.IsCancellationRequested)
                {
                    source.StopProcessing();
                }
            };
            source.Process();
            token.ThrowIfCancellationRequested();
            return (samples, DurationMs(source), source.EventsLost);
        }
        catch (Exception e) when (Unreadable(e, path, writing: false) is { } refused)
        {
            throw refused;
        }
    }

    /// <summary>Pass 2: every sample's stack into stacks, through an ETLX file at scratch that is always deleted.</summary>
    private static void ReadStacks(string path, string scratch, StackAggregator stacks, CancellationToken token)
    {
        try
        {
            // Always the explicit path: TraceLog's default (path + ".etlx") deletes and replaces
            // whatever is next to the input.
            TraceLog.CreateFromEventPipeDataFile(path, scratch);
            if (!OperatingSystem.IsWindows())
            {
                File.SetUnixFileMode(scratch, UnixFileMode.UserRead | UnixFileMode.UserWrite);
            }

            using var log = new TraceLog(scratch);
            var names = new Dictionary<CodeAddressIndex, Frame>();
            var frames = new List<Frame>();
            long events = 0;
            foreach (var e in log.Events)
            {
                if (++events % CheckEvery == 0)
                {
                    token.ThrowIfCancellationRequested();
                }

                if (e is not ClrThreadSampleTraceData sample)
                {
                    continue;
                }

                frames.Clear();
                for (var f = e.CallStack(); f is not null; f = f.Caller)
                {
                    frames.Add(FrameOf(f.CodeAddress, names));
                }

                stacks.Add(e.ThreadID, sample.Type == ClrThreadSampleType.Managed, frames);
            }
        }
        catch (Exception e) when (Unreadable(e, path, writing: true) is { } refused)
        {
            throw refused;
        }
        finally
        {
            Delete(scratch);
            Delete(scratch + ".new");
        }
    }

    /// <summary>A code address's frame, named once per address.</summary>
    private static Frame FrameOf(TraceCodeAddress address, Dictionary<CodeAddressIndex, Frame> names)
    {
        if (!names.TryGetValue(address.CodeAddressIndex, out var frame))
        {
            var method = address.FullMethodName;
            var module = address.ModuleName;
            frame = new Frame(
                string.IsNullOrEmpty(method) ? null : MethodNames.Display(method),
                string.IsNullOrEmpty(module) ? null : Names.Cut(module));
            names[address.CodeAddressIndex] = frame;
        }

        return frame;
    }

    private static long DurationMs(TraceEventSource source)
    {
        var ms = source.SessionDuration.TotalMilliseconds;
        return double.IsFinite(ms) && ms > 0 ? (long)ms : 0;
    }

    /// <summary>
    /// The error for what reading the trace at path threw, or null to let it through:
    /// TRACE_UNSUPPORTED for a file that can't be opened (permissions) or that the parser can't
    /// read (its message isn't echoed: it can quote the file), HELPER_FAILED with only the type for
    /// anything else the parser threw. While writing the ETLX file, an I/O or access error is about
    /// the scratch file, and passes with its message (a path and the OS's text).
    /// </summary>
    internal static HelperException? Unreadable(Exception e, string path, bool writing) => e switch
    {
        HelperException or OperationCanceledException => null,
        IOException or UnauthorizedAccessException when writing && e is not EndOfStreamException => null,
        UnauthorizedAccessException => new HelperException(
            ErrorCodes.TraceUnsupported,
            "can't read " + path + ": permission denied",
            "check the file's permissions, or copy it somewhere you can read"),
        FormatException or EndOfStreamException or InvalidDataException or IOException => Unsupported(
            "not a .nettrace eyedbg can read, or cut off (did the process end abruptly?)"),
        _ => new HelperException(ErrorCodes.HelperFailed, "the trace parser failed: " + e.GetType().Name),
    };

    private static HelperException Unsupported(string message) => new(
        ErrorCodes.TraceUnsupported,
        message,
        "trace a running .NET process with 'eyedbg dotnet trace --pid N', or open the file in PerfView");

    private static void Delete(string path)
    {
        try
        {
            File.Delete(path);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            // eyedbg removes it again after the call.
        }
    }
}
