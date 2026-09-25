// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>A stack frame as a summary shows it: the method's display name (null: unresolved) and its module.</summary>
internal readonly record struct Frame(string? Method, string? Module);

/// <summary>
/// Adds up CPU samples by method. A sample in managed code counts for the method at the top of
/// its stack (exclusive) and once for every method in it, however often a recursive one
/// appears (inclusive); any other sample (waiting, or in native or runtime code) counts for the
/// managed method at its top: where the thread left managed code (waiting). Frames without a
/// method name count as "(unresolved)" in their module. Time O(frames), memory O(distinct methods).
/// </summary>
internal sealed class StackAggregator
{
    public const string Unresolved = "(unresolved)";

    private readonly Dictionary<Frame, long> _exclusive = [];
    private readonly Dictionary<Frame, long> _inclusive = [];
    private readonly Dictionary<Frame, long> _waiting = [];
    private readonly HashSet<Frame> _seen = [];
    private readonly HashSet<int> _threads = [];

    public long Samples { get; private set; }

    public long Managed { get; private set; }

    public long UnresolvedFrames { get; private set; }

    /// <summary>Adds one sample of a thread: in managed code or not, its frames from the top of the stack down.</summary>
    public void Add(int threadId, bool managed, IEnumerable<Frame> topFirst)
    {
        Samples++;
        if (managed)
        {
            Managed++;
        }

        _threads.Add(threadId);
        _seen.Clear();
        var top = true;
        foreach (var f in topFirst)
        {
            var key = f.Method is null ? new Frame(Unresolved, f.Module) : f;
            if (f.Method is null)
            {
                UnresolvedFrames++;
            }

            if (top)
            {
                Increment(managed ? _exclusive : _waiting, key);
                top = false;
            }

            if (managed && _seen.Add(key))
            {
                Increment(_inclusive, key);
            }
        }
    }

    /// <summary>The summary: each list's top rows by samples (ties by method, then module), while the budget lasts.</summary>
    public CpuSummary Result(int top, ResultBudget budget)
    {
        var (exclusive, exclusiveOmitted) = Rows(_exclusive, top, budget);
        var (inclusive, inclusiveOmitted) = Rows(_inclusive, top, budget);
        var (waiting, waitingOmitted) = Rows(_waiting, top, budget);
        return new CpuSummary(
            Samples,
            Managed,
            Samples - Managed,
            _threads.Count,
            UnresolvedFrames,
            exclusive,
            exclusiveOmitted,
            inclusive,
            inclusiveOmitted,
            waiting,
            waitingOmitted);
    }

    private static void Increment(Dictionary<Frame, long> counts, Frame key)
    {
        counts.TryGetValue(key, out var n);
        counts[key] = n + 1;
    }

    private static (List<MethodSamples> Rows, int Omitted) Rows(Dictionary<Frame, long> counts, int top, ResultBudget budget)
    {
        var rows = new List<MethodSamples>(Math.Min(top, counts.Count));
        foreach (var (frame, samples) in counts
            .OrderByDescending(kv => kv.Value)
            .ThenBy(kv => kv.Key.Method, StringComparer.Ordinal)
            .ThenBy(kv => kv.Key.Module, StringComparer.Ordinal)
            .Take(top))
        {
            var row = new MethodSamples(frame.Method!, string.IsNullOrEmpty(frame.Module) ? null : frame.Module, samples);
            if (budget.TryAdd(row, ProtocolJson.Default.MethodSamples))
            {
                rows.Add(row);
            }
        }

        return (rows, counts.Count - rows.Count);
    }
}
