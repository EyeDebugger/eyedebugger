// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>Where an object lives: a generation, or the large, pinned or frozen object heap.</summary>
internal enum Gen
{
    Gen0,
    Gen1,
    Gen2,
    Loh,
    Poh,
    Frozen,
    Unknown,
}

/// <summary>What the heap statistics need of an object: its type (method table), size and generation.</summary>
internal readonly record struct HeapObject(ulong MethodTable, ulong Size, Gen Generation);

/// <summary>A heap's live objects (valid, not free space) and their types' names.</summary>
internal interface IHeapSource
{
    IEnumerable<HeapObject> Objects();

    /// <summary>A method table's type name, or null when it can't be read.</summary>
    string? TypeName(ulong methodTable);
}

/// <summary>
/// Per-type and per-generation totals of a heap: one pass over the objects, O(objects) time and
/// O(distinct types) memory; names are read once per method table.
/// </summary>
internal sealed class HeapStats
{
    /// <summary>How often the walk checks for cancellation.</summary>
    public const int CheckEvery = 4096;

    private static readonly string[] _generationNames = ["gen0", "gen1", "gen2", "loh", "poh", "frozen", "unknown"];

    private readonly long[] _genObjects = new long[_generationNames.Length];
    private readonly long[] _genBytes = new long[_generationNames.Length];
    private readonly Dictionary<string, TypeTotal> _byName = new(StringComparer.Ordinal);

    public long Objects { get; private set; }

    public long Bytes { get; private set; }

    /// <summary>Totals by type name; a name some method tables share (several loads of a type) adds up.</summary>
    public IReadOnlyDictionary<string, TypeTotal> ByName => _byName;

    /// <summary>Walks every object once.</summary>
    public static HeapStats Walk(IHeapSource source, CancellationToken cancellationToken)
    {
        var byMethodTable = new Dictionary<ulong, (long Count, long Bytes)>();
        var stats = new HeapStats();
        long seen = 0;
        foreach (var o in source.Objects())
        {
            if (++seen % CheckEvery == 0)
            {
                cancellationToken.ThrowIfCancellationRequested();
            }

            var size = (long)o.Size;
            stats.Objects++;
            stats.Bytes += size;
            stats._genObjects[(int)o.Generation]++;
            stats._genBytes[(int)o.Generation] += size;
            byMethodTable.TryGetValue(o.MethodTable, out var t);
            byMethodTable[o.MethodTable] = (t.Count + 1, t.Bytes + size);
        }

        foreach (var (mt, t) in byMethodTable)
        {
            var name = Names.Cut(source.TypeName(mt) ?? "<unknown type " + Names.Hex(mt) + ">");
            stats._byName.TryGetValue(name, out var total);
            stats._byName[name] = new TypeTotal(total.Count + t.Count, total.Bytes + t.Bytes, [.. total.MethodTables ?? [], mt]);
        }

        return stats;
    }

    /// <summary>The generations in order; "unknown" only when an object's generation couldn't be told.</summary>
    public List<GenerationStat> Generations()
    {
        var list = new List<GenerationStat>(_generationNames.Length);
        for (int i = 0; i < _generationNames.Length; i++)
        {
            if (i == (int)Gen.Unknown && _genObjects[i] == 0)
            {
                continue;
            }

            list.Add(new GenerationStat(_generationNames[i], _genObjects[i], _genBytes[i]));
        }

        return list;
    }

    /// <summary>
    /// The types whose name contains filter (all without one), biggest first (ties by name), at most
    /// top of them and what fits the budget; typeCount is how many matched.
    /// </summary>
    public (int TypeCount, List<TypeStat> Types) Top(int top, string? filter, ResultBudget budget)
    {
        var matching = ByName
            .Where(kv => string.IsNullOrEmpty(filter) || kv.Key.Contains(filter, StringComparison.Ordinal))
            .Select(kv => new TypeStat(kv.Key, kv.Value.Count, kv.Value.Bytes))
            .OrderByDescending(t => t.Bytes)
            .ThenBy(t => t.Name, StringComparer.Ordinal)
            .ToList();
        var kept = new List<TypeStat>(Math.Min(top, matching.Count));
        foreach (var t in matching)
        {
            if (kept.Count >= top)
            {
                break;
            }

            if (!budget.TryAdd(t, ProtocolJson.Default.TypeStat))
            {
                break;
            }

            kept.Add(t);
        }

        return (matching.Count, kept);
    }
}

/// <summary>A type's totals and the method tables that carry its name.</summary>
internal readonly record struct TypeTotal(long Count, long Bytes, ulong[] MethodTables);

/// <summary>A ClrMD heap as an <see cref="IHeapSource"/>.</summary>
internal sealed class ClrHeapSource : IHeapSource
{
    private readonly ClrHeap _heap;
    private readonly Dictionary<ulong, ClrType> _types = [];

    public ClrHeapSource(ClrHeap heap)
    {
        _heap = heap;
    }

    public IEnumerable<HeapObject> Objects()
    {
        foreach (var segment in _heap.Segments)
        {
            foreach (var obj in segment.EnumerateObjects())
            {
                if (obj.Type is not { } type || type.IsFree)
                {
                    continue;
                }

                _types.TryAdd(type.MethodTable, type);
                yield return new HeapObject(type.MethodTable, obj.Size, GenOf(segment.GetGeneration(obj.Address)));
            }
        }
    }

    public string? TypeName(ulong methodTable) =>
        (_types.TryGetValue(methodTable, out var t) ? t : _heap.GetTypeByMethodTable(methodTable))?.Name;

    internal static Gen GenOf(Generation g) => g switch
    {
        Generation.Generation0 => Gen.Gen0,
        Generation.Generation1 => Gen.Gen1,
        Generation.Generation2 => Gen.Gen2,
        Generation.Large => Gen.Loh,
        Generation.Pinned => Gen.Poh,
        Generation.Frozen => Gen.Frozen,
        _ => Gen.Unknown,
    };
}
