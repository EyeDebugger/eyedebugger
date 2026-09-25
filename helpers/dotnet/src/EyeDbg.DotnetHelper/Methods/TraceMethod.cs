// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The trace method: records an EventPipe trace (profile cpu or gc) of a process for durationMs
/// into a file at an absolute path eyedbg chose (a fresh name in its private directory), then
/// {bytes, elapsedMs, endReason}.
/// </summary>
internal static class TraceMethod
{
    public const string Method = "trace";

    public const long MinDurationMs = 1000;
    public const long MaxDurationMs = 5L * 60 * 1000;

    public static MethodHandler Handler() =>
        async (parameters, _, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.TraceParams));
            var result = await TraceCollector.RunAsync(p, cancellationToken).ConfigureAwait(false);
            return new Reply(result, ProtocolJson.Default.TraceResult);
        };

    internal static TraceParams Validate(TraceParams p)
    {
        if (p.Pid <= 0)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "pid must be positive");
        }

        if (TraceProfiles.Of(p.Profile) is null)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "profile must be cpu or gc");
        }

        if (p.DurationMs is < MinDurationMs or > MaxDurationMs)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "durationMs must be 1000 to 300000");
        }

        if (!Path.IsPathFullyQualified(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path must be absolute");
        }

        if (PathChecks.Exists(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path exists: " + p.Path);
        }

        return p;
    }
}
