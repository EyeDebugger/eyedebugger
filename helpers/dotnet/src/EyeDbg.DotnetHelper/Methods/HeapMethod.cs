// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The heap method: what fills a dump's managed heap (types by size, generations) and, with
/// gcroot, why a type's objects or an object are alive. Never field values or string contents.
/// </summary>
internal static class HeapMethod
{
    public const string Method = "heap";

    public const int MaxTop = 1000;
    public const int MaxPaths = 20;

    public static MethodHandler Handler() =>
        (parameters, _, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.HeapParams));

            // ClrMD's calls are synchronous: run on the thread pool so stdin's end still cancels.
            return Task.Run(() => new Reply(Run(p, cancellationToken), ProtocolJson.Default.HeapResult), cancellationToken);
        };

    internal static HeapParams Validate(HeapParams p)
    {
        AnalysisParams.Check(p.Path, p.TimeoutMs);
        if (p.Top is < 1 or > MaxTop)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "top must be 1 to 1000");
        }

        if (p.Paths is < 1 or > MaxPaths)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "paths must be 1 to 20");
        }

        if (p.Gcroot is { } g && (g.Type is null) == (g.Address is null))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "gcroot needs exactly one of type and address");
        }

        return p;
    }

    private static HeapResult Run(HeapParams p, CancellationToken cancellationToken)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(TimeSpan.FromMilliseconds(p.TimeoutMs));
        using var dump = DumpLoader.Open(p.Path, p.TrustRecordedRuntime);

        HeapStats stats;
        try
        {
            stats = HeapStats.Walk(new ClrHeapSource(dump.Runtime.Heap), deadline.Token);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw AnalysisParams.TimedOut(p.TimeoutMs);
        }

        if (stats.Objects == 0)
        {
            throw new HelperException(
                ErrorCodes.DumpUnsupported,
                "no managed heap in this dump (a mini or triage dump?)",
                "take one with --type heap");
        }

        var budget = new ResultBudget();
        var gcroot = p.Gcroot is { } q ? RootPaths.Find(dump, stats, q, p.Paths, budget, deadline.Token, cancellationToken) : null;
        cancellationToken.ThrowIfCancellationRequested();
        var (typeCount, types) = stats.Top(p.Top, p.TypeFilter, budget);
        return new HeapResult(dump.Info, stats.Objects, stats.Bytes, stats.Generations(), typeCount, types, typeCount - types.Count, gcroot);
    }
}
