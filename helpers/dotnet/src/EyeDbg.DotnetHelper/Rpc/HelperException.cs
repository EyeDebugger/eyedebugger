// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>
/// A user-facing error: a stable code (<see cref="ErrorCodes"/>), a message and an optional hint.
/// Messages never carry data read from a target process.
/// </summary>
internal sealed class HelperException : Exception
{
    public HelperException(string code, string message, string? hint = null)
        : base(message)
    {
        Code = code;
        Hint = hint;
    }

    public string Code { get; }

    public string? Hint { get; }
}
