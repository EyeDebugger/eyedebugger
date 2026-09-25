// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>Creates the file a trace is written to.</summary>
internal static class TraceFile
{
    /// <summary>
    /// Creates path for writing: only if nothing is there (O_EXCL: an existing file or symlink
    /// fails, nothing is followed or truncated), readable and writable by its owner only (Unix;
    /// on Windows it inherits the directory's ACL).
    /// </summary>
    public static FileStream Create(string path)
    {
        var options = new FileStreamOptions
        {
            Mode = FileMode.CreateNew,
            Access = FileAccess.Write,
            Share = FileShare.None,
        };
        if (!OperatingSystem.IsWindows())
        {
            options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
        }

        try
        {
            return new FileStream(path, options);
        }
        catch (Exception e) when ((e is IOException or UnauthorizedAccessException) && PathChecks.Exists(path))
        {
            // Windows reports an existing directory as access denied.
            throw new HelperException(ErrorCodes.InvalidRequest, "path exists: " + path);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            throw new HelperException(ErrorCodes.HelperFailed, "creating the trace file failed: " + e.Message);
        }
    }
}
