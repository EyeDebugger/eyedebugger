// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Counters;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The counters method: System.Runtime counters of a process, one counters.sample
/// notification per interval, then {samples, endReason}. durationMs 0 runs until stdin closes.
/// </summary>
internal static class CountersMethod
{
    public const string Method = "counters";
    public const string SampleNotification = "counters.sample";

    /// <summary>The longest collection (a day); eyedbg asks for at most an hour.</summary>
    public const long MaxDurationMs = 24L * 60 * 60 * 1000;

    public static MethodHandler Handler() =>
        async (parameters, notifier, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.CountersParams));
            var result = await CounterSession.RunAsync(
                p,
                sample => notifier.Notify(SampleNotification, sample, ProtocolJson.Default.CounterSample),
                cancellationToken).ConfigureAwait(false);
            return new Reply(result, ProtocolJson.Default.CountersResult);
        };

    internal static CountersParams Validate(CountersParams p)
    {
        if (p.Pid <= 0)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "pid must be positive");
        }

        if (p.IntervalSec is < 1 or > 60)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "intervalSec must be 1 to 60");
        }

        if (p.DurationMs is < 0 or > MaxDurationMs)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "durationMs must be 0 to a day");
        }

        return p;
    }
}
