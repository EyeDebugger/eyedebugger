// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"slices"
	"strings"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// setCaps stores the capabilities the adapter declared in initialize.
func (s *Session) setCaps(caps godap.Capabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.caps, s.capsKnown = caps, true
}

// capsLocked returns the adapter's capabilities and whether it declared
// them yet (not before it answered initialize).
func (s *Session) capsLocked() (godap.Capabilities, bool) {
	return s.caps, s.capsKnown
}

// unsupported is the error for something the session's adapter can't do.
func (s *Session) unsupported(what string) error {
	return api.NewError(api.CodeUnsupported, "the "+s.Lang+" debug adapter can't "+what,
		"see 'eyedbg help "+s.helpTopic(what)+"' for what works instead")
}

// helpTopic names the help page about what.
func (*Session) helpTopic(what string) string {
	switch {
	case strings.Contains(what, "exception"):
		return "bp exceptions"
	case strings.Contains(what, "set"), strings.Contains(what, "change"):
		return "set"
	default:
		return "bp add"
	}
}

// checkBreakpointCapsLocked refuses a breakpoint the adapter can't place.
// Before the adapter declared its capabilities nothing is refused.
func (s *Session) checkBreakpointCapsLocked(spec api.BreakpointSpec) error {
	caps, known := s.capsLocked()
	if !known {
		return nil
	}

	switch {
	case spec.Function != "" && !caps.SupportsFunctionBreakpoints:
		return s.unsupported("set function breakpoints")
	case spec.Condition != "" && !caps.SupportsConditionalBreakpoints:
		return s.unsupported("evaluate breakpoint conditions (--if)")
	default:
		return nil
	}
}

// checkFiltersLocked refuses exception filters the adapter doesn't declare.
func (s *Session) checkFiltersLocked(filters []string) error {
	caps, known := s.capsLocked()
	if !known {
		return nil
	}

	ids := make([]string, 0, len(caps.ExceptionBreakpointFilters))
	for _, f := range caps.ExceptionBreakpointFilters {
		ids = append(ids, f.Filter)
	}

	for _, f := range filters {
		if slices.Contains(ids, f) {
			continue
		}

		if len(ids) == 0 {
			return s.unsupported("stop at exceptions")
		}

		return api.NewError(api.CodeUnsupported,
			"the "+s.Lang+" debug adapter has no exception filter "+f+" (its filters: "+strings.Join(ids, ", ")+")",
			"see 'eyedbg help bp exceptions'")
	}

	return nil
}
