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
