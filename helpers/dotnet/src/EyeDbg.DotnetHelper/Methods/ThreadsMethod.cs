// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The threads method: a dump's managed threads (ids, flags, top frames with source lines) and
/// its contended locks with their owners. Never thread names or exception messages.
/// </summary>
internal static class ThreadsMethod
{
    public const string Method = "threads";

    public const int MaxFrames = 64;

    public static MethodHandler Handler() =>
        (parameters, _, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.ThreadsParams));

            // ClrMD's calls are synchronous: run on the thread pool so stdin's end still cancels.
            return Task.Run(() => new Reply(Run(p, cancellationToken), ProtocolJson.Default.ThreadsResult), cancellationToken);
        };

    internal static ThreadsParams Validate(ThreadsParams p)
    {
        AnalysisParams.Check(p.Path, p.TimeoutMs);
        if (p.Frames is < 1 or > MaxFrames)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "frames must be 1 to 64");
        }

        return p;
    }

    private static ThreadsResult Run(ThreadsParams p, CancellationToken cancellationToken)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(TimeSpan.FromMilliseconds(p.TimeoutMs));
        using var dump = DumpLoader.Open(p.Path, p.TrustRecordedRuntime);
        try
        {
            return ThreadStacks.Read(dump, p.Frames, deadline.Token);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw AnalysisParams.TimedOut(p.TimeoutMs);
        }
    }
}
