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
