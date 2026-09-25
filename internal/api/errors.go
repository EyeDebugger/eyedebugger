// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "errors"

// Code is a stable, machine-readable error code (docs/DESIGN.md §5). Codes
// never change meaning once released.
type Code string

// Error codes.
const (
	CodeInvalidRequest   Code = "INVALID_REQUEST"
	CodeUnknownMethod    Code = "UNKNOWN_METHOD"
	CodeUnauthorized     Code = "UNAUTHORIZED"
	CodeDaemonNotRunning Code = "DAEMON_NOT_RUNNING"
	CodeDaemonStart      Code = "DAEMON_START_FAILED"
	CodeVersionMismatch  Code = "VERSION_MISMATCH"
	CodeSessionsActive   Code = "SESSIONS_ACTIVE"
	CodeNoSession        Code = "NO_SESSION"
	CodeNotStopped       Code = "NOT_STOPPED"
	CodeNotRunning       Code = "NOT_RUNNING"
	CodeSessionExited    Code = "SESSION_EXITED"
	CodeAdapterMissing   Code = "ADAPTER_NOT_INSTALLED"
	CodeAdapterFailed    Code = "ADAPTER_ERROR"
	CodeBuildFailed      Code = "BUILD_FAILED"
	CodeInternal         Code = "INTERNAL"
	// CodeLeaseHeld: another client holds the session's control lease.
	CodeLeaseHeld Code = "LEASE_HELD"
	// CodeNotOwner: the breakpoint belongs to another client.
	CodeNotOwner Code = "NOT_OWNER"
	// CodeUnsupported: the session's debug adapter can't do what was asked.
	CodeUnsupported Code = "UNSUPPORTED_BY_ADAPTER"
	// CodeSideEffects: the expression would change the program; evaluating
	// it needs allowSideEffects.
	CodeSideEffects Code = "SIDE_EFFECTS"
	// CodeAttachFailed: attaching to a running process failed.
	CodeAttachFailed Code = "ATTACH_FAILED"
	// CodeNoTestHost: a test run ended, or can't run, without a test host
	// to debug.
	CodeNoTestHost Code = "NO_TEST_HOST"
	// CodeAnchorNotFound: no line of the file holds a breakpoint's anchor
	// text.
	CodeAnchorNotFound Code = "ANCHOR_NOT_FOUND"
	// CodeAnchorAmbiguous: several lines of the file hold a breakpoint's
	// anchor text.
	CodeAnchorAmbiguous Code = "ANCHOR_AMBIGUOUS"
	// CodeHelperNotFound: a side helper (or the runtime to run it) isn't
	// installed where eyedbg looks for it.
	CodeHelperNotFound Code = "HELPER_NOT_FOUND"
	// CodeHelperMismatch: the side helper speaks another protocol version
	// than this eyedbg.
	CodeHelperMismatch Code = "HELPER_MISMATCH"
	// CodeHelperFailed: the side helper crashed, exited, broke protocol or
	// didn't finish in time.
	CodeHelperFailed Code = "HELPER_FAILED"
	// CodeNotDotnet: the target process has no .NET diagnostics endpoint and
	// shows no sign of being a .NET process, or the session debugs another
	// language.
	CodeNotDotnet Code = "NOT_DOTNET"
	// CodeDiagnosticsDisabled: the target is a .NET process whose
	// diagnostics endpoint is off or out of reach.
	CodeDiagnosticsDisabled Code = "DIAGNOSTICS_DISABLED"
	// CodeDiagnosticsTimeout: the target didn't start a diagnostics session,
	// or sent no data, in time (paused, stopped under a debugger, or hung).
	CodeDiagnosticsTimeout Code = "DIAGNOSTICS_TIMEOUT"
	// CodeDumpUnsupported: the dump can't be analyzed here — not a dump,
	// truncated, no .NET runtime in it, taken on another OS or architecture,
	// or (for a heap analysis) no managed heap in it.
	CodeDumpUnsupported Code = "DUMP_UNSUPPORTED"
	// CodeDumpRuntimeMissing: the .NET runtime a dump was taken with isn't
	// installed here, so its analysis library can't be loaded.
	CodeDumpRuntimeMissing Code = "DUMP_RUNTIME_MISSING"
	// CodeDumpFailed: the target's runtime couldn't write the dump, or the
	// dump it reported isn't there.
	CodeDumpFailed Code = "DUMP_FAILED"
	// CodeTraceUnsupported: the trace can't be read — not a .nettrace
	// eyedbg can parse, cut off (the process ended abruptly), or not
	// readable by the user. A readable trace with nothing in it isn't an
	// error.
	CodeTraceUnsupported Code = "TRACE_UNSUPPORTED"
)

// Error is a user-facing error: a stable code, a message and an optional hint
// telling the reader what to do next.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// NewError returns an *Error.
func NewError(code Code, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// CodeOf returns the code of the first *Error in err's chain, or "" if none.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}

	return ""
}
