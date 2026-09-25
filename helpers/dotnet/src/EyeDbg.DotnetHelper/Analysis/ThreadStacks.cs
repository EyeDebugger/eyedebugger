// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Collections.Immutable;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>A monitor lock as read from a sync block, before its owner is resolved.</summary>
internal readonly record struct LockData(ulong Object, string Type, ulong HoldingThread, bool Held, int Waiting);

/// <summary>
/// What a dump's managed threads were doing: their top frames and the contended locks (held
/// monitors and their waiters). Which lock a waiting thread waits on isn't recorded by the runtime.
/// </summary>
internal static class ThreadStacks
{
    /// <summary>Frames read per thread at most (the rest aren't counted).</summary>
    public const int MaxFramesRead = 1024;

    /// <summary>Reads threads and locks from the runtime.</summary>
    public static ThreadsResult Read(LoadedDump dump, int frames, CancellationToken cancellationToken)
    {
        var runtime = dump.Runtime;
        var heap = runtime.Heap;
        using var lines = new SourceLines();
        var threads = new List<(ThreadInfo Info, ulong Address)>();
        foreach (var t in runtime.Threads)
        {
            cancellationToken.ThrowIfCancellationRequested();
            var (kept, omitted) = Take(
                t.EnumerateStackTrace(false, MaxFramesRead).Where(f => f.Kind != ClrStackFrameKind.Unknown),
                frames,
                (f, i) => Frame(f, leaf: i == 0, lines));
            var (background, threadpool) = Flags(t.State);
            threads.Add((new ThreadInfo(
                t.ManagedThreadId,
                t.OSThreadId,
                t.IsAlive,
                background,
                threadpool,
                t.IsFinalizer,
                t.IsGc,
                t.CurrentException?.Type?.Name is { } ex ? Names.Cut(ex) : null,
                kept,
                omitted), t.Address));
        }

        // Sync blocks name objects on the heap: a mini or triage dump has none to read.
        bool locksAvailable = heap.EnumerateObjects().Any();
        var locks = new List<LockData>();
        if (locksAvailable)
        {
            foreach (var sb in heap.EnumerateSyncBlocks())
            {
                if (sb.IsMonitorHeld || sb.WaitingThreadCount > 0)
                {
                    locks.Add(new LockData(sb.Object, Names.Cut(heap.GetObjectType(sb.Object)?.Name ?? "?"), sb.HoldingThreadAddress, sb.IsMonitorHeld, sb.WaitingThreadCount));
                }
            }
        }

        return Build(dump.Info, threads, locks, locksAvailable, new ResultBudget());
    }

    /// <summary>
    /// The result: locks with their owner's managed id; threads ordered lock owners first, then
    /// threads with managed frames, then the rest (each by managed id), while the budget lasts.
    /// </summary>
    internal static ThreadsResult Build(RuntimeInfo runtime, IReadOnlyList<(ThreadInfo Info, ulong Address)> threads, IReadOnlyList<LockData> locks, bool locksAvailable, ResultBudget budget)
    {
        var byAddress = new Dictionary<ulong, int>();
        foreach (var (info, address) in threads)
        {
            byAddress.TryAdd(address, info.ManagedId);
        }

        var lockList = new List<LockInfo>();
        var owners = new HashSet<int>();
        foreach (var l in locks.OrderBy(l => l.Object))
        {
            int? owner = l.Held && byAddress.TryGetValue(l.HoldingThread, out var id) ? id : null;
            if (owner is { } o)
            {
                owners.Add(o);
            }

            var info = new LockInfo(Names.Hex(l.Object), l.Type, owner, l.Waiting);
            if (budget.TryAdd(info, ProtocolJson.Default.LockInfo))
            {
                lockList.Add(info);
            }
        }

        var ordered = threads
            .Select(t => t.Info)
            .OrderBy(t => owners.Contains(t.ManagedId) ? 0 : t.Frames.Any(f => f.Kind == "managed") ? 1 : 2)
            .ThenBy(t => t.ManagedId);
        var kept = new List<ThreadInfo>();
        foreach (var t in ordered)
        {
            if (budget.TryAdd(t, ProtocolJson.Default.ThreadInfo))
            {
                kept.Add(t);
            }
        }

        return new ThreadsResult(runtime, kept, threads.Count - kept.Count, lockList, locksAvailable);
    }

    /// <summary>A thread's background and threadpool (worker or I/O completion) flags.</summary>
    internal static (bool Background, bool Threadpool) Flags(ClrThreadState state) =>
        ((state & ClrThreadState.TS_Background) != 0, (state & (ClrThreadState.TS_TPWorkerThread | ClrThreadState.TS_CompletionPortThread)) != 0);

    /// <summary>The first n items, converted, and how many more there were.</summary>
    internal static (List<TOut> Kept, int Omitted) Take<TIn, TOut>(IEnumerable<TIn> items, int n, Func<TIn, int, TOut> convert)
    {
        var kept = new List<TOut>(n);
        int total = 0;
        foreach (var item in items)
        {
            if (kept.Count < n)
            {
                kept.Add(convert(item, total));
            }

            total++;
        }

        return (kept, total - kept.Count);
    }

    /// <summary>
    /// The IL offset of the map entry holding ip, or null: no map in the dump (Windows heap dumps
    /// lack the JIT's debug info), ip outside it, or a prolog/epilog/unmapped entry. Unlike
    /// ClrMethod.GetILOffset, never a guess (it answers 0 without a map and the second entry's
    /// offset for an ip outside it).
    /// </summary>
    internal static int? ILOffset(ImmutableArray<ILToNativeMap> map, ulong ip)
    {
        if (map.IsDefaultOrEmpty)
        {
            return null;
        }

        foreach (var entry in map)
        {
            if (entry.StartAddress <= ip && ip <= entry.EndAddress)
            {
                return entry.ILOffset >= 0 ? entry.ILOffset : null;
            }
        }

        return null;
    }

    private static FrameInfo Frame(ClrStackFrame f, bool leaf, SourceLines lines)
    {
        if (f.Kind != ClrStackFrameKind.ManagedMethod || f.Method is not { } method)
        {
            return new FrameInfo("runtime", Names.Cut(f.FrameName ?? "?"), null, null, null, null);
        }

        // A caller's instruction pointer is the return address, just after its call.
        var ip = leaf || f.InstructionPointer == 0 ? f.InstructionPointer : f.InstructionPointer - 1;
        int? il = ILOffset(method.ILOffsetMap, ip);
        var modulePath = method.Type?.Module?.Name;
        var line = il is { } offset ? lines.Find(modulePath, method.MetadataToken, offset) : null;
        return new FrameInfo(
            "managed",
            Names.Cut(method.Signature ?? method.Name ?? "?"),
            modulePath is null ? null : Names.Cut(Path.GetFileName(modulePath)),
            il,
            line?.File,
            line?.Line);
    }
}
