// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Globalization;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>Checks shared by the dump analyses (heap, threads).</summary>
internal static class AnalysisParams
{
    public const long MinTimeoutMs = 1000;
    public const long MaxTimeoutMs = 60L * 60 * 1000;

    /// <summary>The dump path must be absolute and a file; the timeout in range.</summary>
    public static void Check(string path, long timeoutMs)
    {
        if (!Path.IsPathFullyQualified(path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path must be absolute");
        }

        if (!File.Exists(path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "no dump file at " + path);
        }

        if (timeoutMs is < MinTimeoutMs or > MaxTimeoutMs)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "timeoutMs must be 1000 to 3600000");
        }
    }

    /// <summary>The error for an analysis that ran out of time.</summary>
    public static HelperException TimedOut(long timeoutMs) => new(
        ErrorCodes.DiagnosticsTimeout,
        "the analysis didn't finish within " + (timeoutMs / 1000.0).ToString("0.###", CultureInfo.InvariantCulture) + "s (a large heap?)",
        "raise --timeout");
}
