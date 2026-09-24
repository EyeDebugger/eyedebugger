// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"encoding/json"
	"fmt"
	"os"

	godap "github.com/google/go-dap"
)

// EnvFakeOptions carries a started adapter's [Options] as JSON.
const EnvFakeOptions = "EYEDBG_TEST_FAKE_OPTIONS"

// Exception filters the fake adapter declares and understands.
const (
	filterAll           = "all"
	filterUserUnhandled = "user-unhandled"
)

// Launch (and attach) argument field names, shared with [UserManifest].
const (
	argProgram = "program"
	argLines   = "lines"
)

// Options change how the fake adapter behaves.
type Options struct {
	// Caps replaces the capabilities it declares (nil: [DefaultCaps]).
	Caps *godap.Capabilities `json:"caps,omitempty"`
	// StepHitsBreakpoints makes a step that lands on a breakpoint whose
	// condition holds stop with reason breakpoint, as netcoredbg does (a
	// breakpoint hit ends the step there).
	StepHitsBreakpoints bool `json:"stepHitsBreakpoints,omitempty"`
	// GlobalsScope marks the Locals scope with presentationHint "locals"
	// and adds a cheap "Globals" scope holding g = 1, as debugpy does.
	GlobalsScope bool `json:"globalsScope,omitempty"`
	// LateVerify answers setBreakpoints with the breakpoints it would
	// verify unverified, and verifies them (breakpoint events, reason
	// changed) when the program next resumes, as netcoredbg does once a
	// module loads.
	LateVerify bool `json:"lateVerify,omitempty"`
	// VerifyOnSetBreakpoints (with LateVerify) also verifies the pending
	// breakpoints when a setBreakpoints request arrives, before answering
	// it: an adapter verifying while another request is in flight.
	VerifyOnSetBreakpoints bool `json:"verifyOnSetBreakpoints,omitempty"`
	// ExitBeforeConnect makes an adapter started with [ConnectArg] exit
	// at once without connecting; HangBeforeConnect makes it wait, without
	// connecting, until it is killed.
	ExitBeforeConnect bool `json:"exitBeforeConnect,omitempty"`
	HangBeforeConnect bool `json:"hangBeforeConnect,omitempty"`
	// SetVariableResult answers setVariable with the new value under
	// "result" instead of "value", as lldb-dap 18-20 do.
	SetVariableResult bool `json:"setVariableResult,omitempty"`
	// PauseAsSignal reports a pause as a stop with reason exception and
	// description "signal SIGSTOP", as lldb-dap does on Linux.
	PauseAsSignal bool `json:"pauseAsSignal,omitempty"`
	// Unanswered lists commands whose requests it never answers: a request
	// held in flight at the adapter until the client gives up on it.
	Unanswered []string `json:"unanswered,omitempty"`
}

// DefaultCaps are the capabilities the fake adapter declares by default:
// netcoredbg's, as far as the fake implements them.
func DefaultCaps() godap.Capabilities {
	return godap.Capabilities{
		SupportsConfigurationDoneRequest: true,
		SupportsConditionalBreakpoints:   true,
		SupportsFunctionBreakpoints:      true,
		SupportsSetVariable:              true,
		SupportsSetExpression:            true,
		SupportsExceptionInfoRequest:     true,
		ExceptionBreakpointFilters: []godap.ExceptionBreakpointsFilter{
			{Filter: filterAll, Label: "All exceptions"},
			{Filter: filterUserUnhandled, Label: "User-unhandled exceptions"},
		},
	}
}

func (o Options) caps() godap.Capabilities {
	if o.Caps != nil {
		return *o.Caps
	}

	return DefaultCaps()
}

// CommandWith is [Command] for an adapter with opts.
func CommandWith(opts Options) (path string, args, env []string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locate the test binary: %w", err)
	}

	raw, err := json.Marshal(opts)
	if err != nil {
		return "", nil, nil, fmt.Errorf("encode fake adapter options: %w", err)
	}

	return exe, []string{noTestsArg}, []string{EnvFakeAdapter + "=1", EnvFakeOptions + "=" + string(raw)}, nil
}

// ProgramArgs are a fake program's launch (or attach) arguments.
type ProgramArgs struct {
	Program     string `json:"program"`
	Lines       int    `json:"lines"`
	StopAtEntry bool   `json:"stopAtEntry,omitempty"`
	// Hang keeps it running after its last line instead of exiting.
	Hang bool `json:"hang,omitempty"`
	// Laps runs lines 1..Lines this many times (0 or 1: once).
	Laps int `json:"laps,omitempty"`
	// Throws are lines that throw a caught Fake.Error when they run.
	Throws []int `json:"throws,omitempty"`
	// ProcessID is the process an attach request attaches to.
	ProcessID int `json:"processId,omitempty"`
	// FailAttach fails configurationDone as netcoredbg does when it can't
	// attach.
	FailAttach bool `json:"failAttach,omitempty"`
}

// Map returns the arguments as a request body.
func (a ProgramArgs) Map() map[string]any {
	m := map[string]any{argProgram: a.Program, argLines: a.Lines, "stopAtEntry": a.StopAtEntry, "hang": a.Hang}

	if a.Laps > 0 {
		m["laps"] = a.Laps
	}

	if len(a.Throws) > 0 {
		m["throws"] = a.Throws
	}

	if a.ProcessID > 0 {
		m["processId"] = a.ProcessID
	}

	if a.FailAttach {
		m["failAttach"] = true
	}

	return m
}

// AttachArguments returns the attach arguments for a fake program of n
// lines, "attached" to process pid. failAttach makes configurationDone fail.
func AttachArguments(program string, n, pid int, failAttach bool) map[string]any {
	return ProgramArgs{Program: program, Lines: n, ProcessID: pid, FailAttach: failAttach}.Map()
}
