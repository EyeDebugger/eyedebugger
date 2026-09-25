// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Counters;

/// <summary>
/// Groups counter values into samples, one per interval: EventCounters report every counter
/// once per interval, so a name seen again starts the next sample. Emits a sample when that
/// happens and on <see cref="Close"/>; nothing after Close. Safe for concurrent use: the event
/// thread adds while the call's thread closes.
/// </summary>
internal sealed class SampleBatcher
{
    private readonly object _lock = new();
    private readonly Func<long> _elapsedMs;
    private readonly Action<CounterSample> _emit;
    private readonly TaskCompletionSource _failed = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private readonly HashSet<string> _names = new(StringComparer.Ordinal);
    private List<CounterValue> _pending = [];
    private long _pendingElapsedMs;
    private bool _closed;

    /// <param name="elapsedMs">Milliseconds since the session started.</param>
    /// <param name="emit">Sends a sample; if it throws, the batcher closes and <see cref="Failed"/> completes.</param>
    public SampleBatcher(Func<long> elapsedMs, Action<CounterSample> emit)
    {
        _elapsedMs = elapsedMs;
        _emit = emit;
    }

    /// <summary>Samples emitted so far.</summary>
    public int Samples { get; private set; }

    /// <summary>Why emitting failed, if it did.</summary>
    public Exception? EmitError { get; private set; }

    /// <summary>Completes when emitting a sample fails.</summary>
    public Task Failed => _failed.Task;

    public void Add(CounterValue value)
    {
        lock (_lock)
        {
            if (_closed)
            {
                return;
            }

            if (_names.Contains(value.Name))
            {
                FlushLocked();
                if (_closed)
                {
                    return;
                }
            }

            if (_pending.Count == 0)
            {
                _pendingElapsedMs = _elapsedMs();
            }

            _pending.Add(value);
            _names.Add(value.Name);
        }
    }

    /// <summary>Emits what is pending (unless flush is false) and stops: later values are dropped.</summary>
    public void Close(bool flush = true)
    {
        lock (_lock)
        {
            if (!_closed)
            {
                if (flush)
                {
                    FlushLocked();
                }

                _closed = true;
            }
        }
    }

    private void FlushLocked()
    {
        if (_pending.Count == 0)
        {
            return;
        }

        var sample = new CounterSample(_pendingElapsedMs, _pending);
        _pending = [];
        _names.Clear();
        try
        {
            _emit(sample);
            Samples++;
        }
        catch (Exception e)
        {
            EmitError = e;
            _closed = true;
            _failed.TrySetResult();
        }
    }
}
