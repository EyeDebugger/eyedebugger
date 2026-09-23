// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strconv"
	"strings"
)

// Methods added for breakpoint kinds, exceptions, attaching and test runs.
const (
	// MethodSet changes a variable of the stopped program.
	MethodSet = "set"
	// MethodBreakpointExceptions reports or sets the caller's exception
	// mode.
	MethodBreakpointExceptions = "bp.exceptions"
	// MethodSessionDetach ends an attached session and leaves its process
	// running.
	MethodSessionDetach = "session.detach"
)

// AttachSpec names the running process to attach to.
type AttachSpec struct {
	PID int `json:"pid"`
}

// TestSpec asks to debug a test run of the project in [LaunchSpec]: Filter
// selects tests (the test runner's syntax), Framework one target framework.
type TestSpec struct {
	Filter    string `json:"filter,omitempty"`
	Framework string `json:"framework,omitempty"`
}

// ExceptionMode is which exceptions stop a client's program.
type ExceptionMode string

// Exception modes.
const (
	// ExceptionsNone stops at no exception the adapter lets pass (some,
	// like netcoredbg, always stop at one that would end the program).
	ExceptionsNone ExceptionMode = "none"
	// ExceptionsUncaught stops at exceptions no user code catches.
	ExceptionsUncaught ExceptionMode = "uncaught"
	// ExceptionsAll stops at every exception thrown.
	ExceptionsAll ExceptionMode = "all"
)

// ExceptionModes returns every exception mode.
func ExceptionModes() []ExceptionMode {
	return []ExceptionMode{ExceptionsNone, ExceptionsUncaught, ExceptionsAll}
}

// ParseExceptionMode checks that s names an exception mode.
func ParseExceptionMode(s string) (ExceptionMode, error) {
	names := make([]string, 0, 3)

	for _, m := range ExceptionModes() {
		if s == string(m) {
			return m, nil
		}

		names = append(names, string(m))
	}

	return "", NewError(CodeInvalidRequest, "unknown exception mode "+strconv.Quote(s), "use "+strings.Join(names, ", "))
}

// ExceptionsParams are the params of [MethodBreakpointExceptions]. An empty
// Mode only reports; Force sets Mode for every client (so exactly Mode
// applies).
type ExceptionsParams struct {
	SessionRef

	Mode  ExceptionMode `json:"mode,omitempty"`
	Force bool          `json:"force,omitempty"`
}

// ClientExceptionMode is one client's exception mode.
type ClientExceptionMode struct {
	Client string        `json:"client"`
	Mode   ExceptionMode `json:"mode"`
}

// ExceptionsResult is the result of [MethodBreakpointExceptions]: every
// client's mode other than none, in the order they were set, and the
// adapter's exception filters that the union of them turns on.
type ExceptionsResult struct {
	SessionID string                `json:"sessionId"`
	Modes     []ClientExceptionMode `json:"modes"`
	Filters   []string              `json:"filters"`
}

// ExceptionInfo describes an exception the program stopped at. Truncated
// means a message or stack trace was cut.
type ExceptionInfo struct {
	ID          string          `json:"id"`
	Description string          `json:"description,omitempty"`
	BreakMode   string          `json:"breakMode,omitempty"`
	Type        string          `json:"type,omitempty"`
	Message     string          `json:"message,omitempty"`
	StackTrace  string          `json:"stackTrace,omitempty"`
	Inner       []ExceptionInfo `json:"inner,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
}

// SetParams are the params of [MethodSet]: assign Value (an expression) to
// Variable (a name or path like order.Total) in Frame.
type SetParams struct {
	SessionRef

	Variable string `json:"variable"`
	Value    string `json:"value"`
	Frame    int    `json:"frame"`
}

// SetResult is the result of [MethodSet]: the variable's new value.
type SetResult struct {
	Variable string `json:"variable"`
	Value    string `json:"value"`
	Type     string `json:"type,omitempty"`
}
