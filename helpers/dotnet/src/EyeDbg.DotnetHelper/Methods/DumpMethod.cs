// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Dumps;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>
/// The dump method: the target's runtime writes a dump of it at an absolute path eyedbg chose
/// (a fresh name in its private directory), then {bytes, elapsedMs}.
/// </summary>
internal static class DumpMethod
{
    public const string Method = "dump";

    public const long MinTimeoutMs = 1000;
    public const long MaxTimeoutMs = 60L * 60 * 1000;

    public static MethodHandler Handler() =>
        async (parameters, _, cancellationToken) =>
        {
            var p = Validate(ProtocolJson.ParseParams(parameters, ProtocolJson.Default.DumpParams));
            var result = await DumpWriter.WriteAsync(p, cancellationToken).ConfigureAwait(false);
            return new Reply(result, ProtocolJson.Default.DumpResult);
        };

    internal static DumpParams Validate(DumpParams p)
    {
        if (p.Pid <= 0)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "pid must be positive");
        }

        if (!DumpWriter.Types.ContainsKey(p.Type))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "type must be heap, mini, triage or full");
        }

        // The runtime resolves a relative path against the target's working directory.
        if (!Path.IsPathFullyQualified(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path must be absolute");
        }

        // createdump truncates an existing file and follows a symlink: the name must be fresh.
        if (Exists(p.Path))
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "path exists: " + p.Path);
        }

        if (p.TimeoutMs is < MinTimeoutMs or > MaxTimeoutMs)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "timeoutMs must be 1000 to 3600000");
        }

        return p;
    }

    /// <summary>Whether anything is at path, a dangling symlink included.</summary>
    private static bool Exists(string path)
    {
        var info = new FileInfo(path);
        return info.Exists || info.LinkTarget is not null || Directory.Exists(path);
    }
}
