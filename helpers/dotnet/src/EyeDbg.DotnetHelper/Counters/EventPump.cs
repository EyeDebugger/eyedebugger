// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using Microsoft.Diagnostics.Tracing;

namespace EyeDbg.DotnetHelper.Counters;

/// <summary>
/// Reads an EventPipe stream to its end on a dedicated background thread and hands every
/// EventCounters event to a callback. The stream is always drained: a target whose events
/// go unread blocks writing them (seen on Windows), and then can't stop the session.
/// </summary>
internal sealed class EventPump
{
    private readonly TaskCompletionSource<Exception?> _done = new(TaskCreationOptions.RunContinuationsAsynchronously);

    private EventPump()
    {
    }

    /// <summary>Completes when the stream ends: null at its end, or what stopped reading it.</summary>
    public Task<Exception?> Done => _done.Task;

    public static EventPump Start(Stream stream, Action<TraceEvent> onCounters)
    {
        var pump = new EventPump();
        var thread = new Thread(() => pump.Run(stream, onCounters))
        {
            IsBackground = true,
            Name = "eyedbg-eventpipe",
        };
        thread.Start();
        return pump;
    }

    private void Run(Stream stream, Action<TraceEvent> onCounters)
    {
        try
        {
            // The constructor already reads the stream's header: keep it off the call's thread.
            using var source = new EventPipeEventSource(stream);
            source.Dynamic.All += e =>
            {
                if (e.EventName == CounterPayload.EventName)
                {
                    onCounters(e);
                }
            };
            source.Process();
            _done.TrySetResult(null);
        }
        catch (Exception e)
        {
            _done.TrySetResult(e);
        }
    }
}
