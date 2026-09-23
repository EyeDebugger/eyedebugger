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

// Options change how the fake adapter behaves.
type Options struct {
	// Caps replaces the capabilities it declares (nil: [DefaultCaps]).
	Caps *godap.Capabilities `json:"caps,omitempty"`
	// StepHitsBreakpoints makes a step that lands on a breakpoint whose
	// condition holds stop with reason breakpoint, as netcoredbg does (a
	// breakpoint hit ends the step there).
	StepHitsBreakpoints bool `json:"stepHitsBreakpoints,omitempty"`
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
			{Filter: "all", Label: "All exceptions"},
			{Filter: "user-unhandled", Label: "User-unhandled exceptions"},
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

	return exe, []string{"-test.run=^$"}, []string{EnvFakeAdapter + "=1", EnvFakeOptions + "=" + string(raw)}, nil
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
	m := map[string]any{"program": a.Program, "lines": a.Lines, "stopAtEntry": a.StopAtEntry, "hang": a.Hang}

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
