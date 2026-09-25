// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>Checks on paths eyedbg hands the helper.</summary>
internal static class PathChecks
{
    /// <summary>Whether anything is at path, a dangling symlink included.</summary>
    public static bool Exists(string path)
    {
        var info = new FileInfo(path);
        return info.Exists || info.LinkTarget is not null || Directory.Exists(path);
    }
}
