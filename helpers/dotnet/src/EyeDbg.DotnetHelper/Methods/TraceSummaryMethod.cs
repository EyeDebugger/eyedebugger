// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The traceSummary method: what a .nettrace file shows — the hottest methods and where threads
/// waited (CPU samples), garbage collections and the most allocated types. Method, module and
/// type names and counts, never values.
/// </summary>
internal static class TraceSummaryMethod
{
    public const string Method = "traceSummary";

    public const int MaxTop = 1000;
    public const long MinTimeoutMs = 1000;
    public const long MaxTimeoutMs = 60L * 60 * 1000;

    public static MethodHandler Handler() =>
        (parameters, _, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.TraceSummaryParams));

            // TraceEvent's calls are synchronous: run on the thread pool so stdin's end still cancels.
            return Task.Run(() => new Reply(TraceSummary.Run(p, cancellationToken), ProtocolJson.Default.TraceSummaryResult), cancellationToken);
        };

    internal static TraceSummaryParams Validate(TraceSummaryParams p)
    {
        if (!Path.IsPathFullyQualified(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path must be absolute");
        }

        if (!File.Exists(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "no trace file at " + p.Path);
        }

        // TraceLog writes scratch + ".new", then deletes scratch and moves the new file there.
        if (!Path.IsPathFullyQualified(p.Scratch))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "scratch must be absolute");
        }

        if (PathChecks.Exists(p.Scratch) || PathChecks.Exists(p.Scratch + ".new"))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "scratch exists: " + p.Scratch);
        }

        if (p.Top is < 1 or > MaxTop)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "top must be 1 to 1000");
        }

        if (p.TimeoutMs is < MinTimeoutMs or > MaxTimeoutMs)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "timeoutMs must be 1000 to 3600000");
        }

        return p;
    }
}
