// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>
/// Adds up garbage collections (per generation, background, induced, pauses) and allocation
/// ticks by type. Memory O(distinct types).
/// </summary>
internal sealed class GcAggregator
{
    public const string UnknownType = "(unknown type)";

    private readonly int[] _generations = new int[3];
    private readonly Dictionary<string, (long Ticks, long Bytes)> _types = new(StringComparer.Ordinal);
    private int _collections;
    private int _background;
    private int _induced;
    private double _pauseTotalMs;
    private double _pauseMaxMs;
    private long _tickBytes;

    public int Collections => _collections;

    public long Ticks { get; private set; }

    /// <summary>
    /// Adds a collection: its generation (0 to 2), whether it ran in the background, the name of
    /// its reason (any starting with "Induced" was asked for by the program), its pause.
    /// </summary>
    public void AddGc(int generation, bool background, string reason, double pauseMs)
    {
        _collections++;
        if (generation is >= 0 and <= 2)
        {
            _generations[generation]++;
        }

        if (background)
        {
            _background++;
        }

        if (reason.StartsWith("Induced", StringComparison.Ordinal))
        {
            _induced++;
        }

        // A value JSON can't carry (NaN, infinite) or a negative pause counts as none.
        var pause = double.IsFinite(pauseMs) && pauseMs > 0 ? pauseMs : 0;
        _pauseTotalMs += pause;
        _pauseMaxMs = Math.Max(_pauseMaxMs, pause);
    }

    /// <summary>Adds an allocation tick: the type allocated and the bytes it accounts for.</summary>
    public void AddTick(string? typeName, long bytes)
    {
        Ticks++;
        var amount = Math.Max(0, bytes);
        _tickBytes = SaturatingAdd(_tickBytes, amount);
        var name = string.IsNullOrEmpty(typeName) ? UnknownType : Names.Cut(typeName);
        _types.TryGetValue(name, out var t);
        _types[name] = (t.Ticks + 1, SaturatingAdd(t.Bytes, amount));
    }

    /// <summary>The collections, or null when there was none.</summary>
    public GcSummary? Gc() => _collections == 0
        ? null
        : new GcSummary(_collections, _generations[0], _generations[1], _generations[2], _background, _induced, _pauseTotalMs, _pauseMaxMs);

    /// <summary>The allocations, or null without a tick: the top types by bytes (ties by name), while the budget lasts.</summary>
    public AllocationSummary? Allocations(int top, ResultBudget budget)
    {
        if (Ticks == 0)
        {
            return null;
        }

        var rows = new List<AllocatedType>(Math.Min(top, _types.Count));
        foreach (var (name, t) in _types
            .OrderByDescending(kv => kv.Value.Bytes)
            .ThenByDescending(kv => kv.Value.Ticks)
            .ThenBy(kv => kv.Key, StringComparer.Ordinal)
            .Take(top))
        {
            var row = new AllocatedType(name, t.Ticks, t.Bytes);
            if (budget.TryAdd(row, ProtocolJson.Default.AllocatedType))
            {
                rows.Add(row);
            }
        }

        return new AllocationSummary(Ticks, _tickBytes, _types.Count, rows, _types.Count - rows.Count);
    }

    private static long SaturatingAdd(long a, long b) => a > long.MaxValue - b ? long.MaxValue : a + b;
}
