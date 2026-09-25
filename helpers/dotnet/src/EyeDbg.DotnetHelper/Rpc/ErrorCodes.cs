// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>
/// Stable error codes shared with eyedbg (internal/api/errors.go, docs/DESIGN.md §5).
/// </summary>
internal static class ErrorCodes
{
    public const string InvalidRequest = "INVALID_REQUEST";
    public const string UnknownMethod = "UNKNOWN_METHOD";
    public const string HelperFailed = "HELPER_FAILED";
    public const string NotDotnet = "NOT_DOTNET";
    public const string DiagnosticsDisabled = "DIAGNOSTICS_DISABLED";
    public const string DiagnosticsTimeout = "DIAGNOSTICS_TIMEOUT";
    public const string DumpUnsupported = "DUMP_UNSUPPORTED";
    public const string DumpRuntimeMissing = "DUMP_RUNTIME_MISSING";
    public const string DumpFailed = "DUMP_FAILED";
    public const string TraceUnsupported = "TRACE_UNSUPPORTED";
}
