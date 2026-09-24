// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// capabilities are what the facade declares in initialize: the adapter's,
// with what eyedbg itself provides forced on (configurationDone, hit
// conditions and logpoints, which the session emulates, terminate), what it
// refuses forced off, and one exception filter per exception mode the
// adapter can serve (modes).
func capabilities(adapter godap.Capabilities, modes []api.ExceptionMode) godap.Capabilities {
	c := adapter

	c.SupportsConfigurationDoneRequest = true
	c.SupportsHitConditionalBreakpoints = true
	c.SupportsLogPoints = true
	c.SupportsTerminateRequest = true

	c.SupportsRestartRequest = false
	c.SupportsDataBreakpoints = false
	c.SupportsStepBack = false
	c.SupportsGotoTargetsRequest = false
	c.SupportsRestartFrame = false
	c.SupportsCancelRequest = false
	c.SupportsTerminateThreadsRequest = false
	c.SupportsWriteMemoryRequest = false
	c.SupportsInstructionBreakpoints = false
	c.SupportsExceptionOptions = false
	c.SupportsExceptionFilterOptions = false
	c.SupportsSteppingGranularity = false
	c.SupportsSingleThreadExecutionRequests = false
	c.SupportsStepInTargetsRequest = false
	// A clipboard evaluate is sent as watch: in clipboard, debugpy drops its
	// truncation, and one big value's response would exceed what eyedbg
	// reads from an adapter. Without it, VS Code copies the value it shows.
	c.SupportsClipboardContext = false
	// Without them VS Code offers Disconnect only: leaving never ends the
	// session (terminate does, under the lease).
	c.SupportTerminateDebuggee = false
	c.SupportSuspendDebuggee = false

	c.ExceptionBreakpointFilters = nil

	for _, m := range modes {
		switch m {
		case api.ExceptionsAll:
			c.ExceptionBreakpointFilters = append(c.ExceptionBreakpointFilters,
				godap.ExceptionBreakpointsFilter{Filter: string(api.ExceptionsAll), Label: "All exceptions"})
		case api.ExceptionsUncaught:
			c.ExceptionBreakpointFilters = append(c.ExceptionBreakpointFilters,
				godap.ExceptionBreakpointsFilter{Filter: string(api.ExceptionsUncaught), Label: "Uncaught exceptions"})
		case api.ExceptionsNone:
		}
	}

	return c
}

// exceptionMode is the mode setExceptionBreakpoints asks for: all if its
// filters name it, else uncaught, else none. Unknown filters are ignored.
func exceptionMode(filters []string) api.ExceptionMode {
	mode := api.ExceptionsNone

	for _, f := range filters {
		switch api.ExceptionMode(f) {
		case api.ExceptionsAll:
			return api.ExceptionsAll
		case api.ExceptionsUncaught:
			mode = api.ExceptionsUncaught
		case api.ExceptionsNone:
		}
	}

	return mode
}
