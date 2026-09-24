// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Ready waits until the session finished starting (ctx bounds the wait) and
// returns the adapter's capabilities. A session still starting when ctx
// ends is NOT_STOPPED; one that exited before its adapter declared any is
// SESSION_EXITED.
func (s *Session) Ready(ctx context.Context) (godap.Capabilities, error) {
	s.waitUntil(ctx, func() bool { return s.state != api.StateStarting })

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == api.StateStarting {
		return godap.Capabilities{}, stateError(s.ID, s.state, "joining needs a session that finished starting")
	}

	caps, known := s.capsLocked()
	if !known {
		return godap.Capabilities{}, stateError(s.ID, api.StateExited, "joining needs a live session")
	}

	return cloneCaps(caps), nil
}

// JoinPoint returns the session's summary and the newest event log seq,
// read together: every event after that seq happened after the summary.
// Every log append runs under mu, so nothing slips in between.
func (s *Session) JoinPoint() (info api.SessionInfo, seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.infoLocked(), s.log.latest()
}

// Follow returns the events after seq since, waiting until one exists or
// ctx ends (then TimedOut and none). Unlike [Session.Events] nothing is
// shaped or cut; at most api.MaxEventsLimit come at a time, and Dropped
// counts those the log no longer holds.
func (s *Session) Follow(ctx context.Context, since int) api.EventsResult {
	return s.log.wait(ctx, api.EventsParams{Since: max(since, 0), Limit: api.MaxEventsLimit})
}

// SupportedExceptionModes returns the exception modes other than none
// whose filters the adapter declares: none while its capabilities are
// unknown or it declares no filters.
func (s *Session) SupportedExceptionModes() []api.ExceptionMode {
	s.mu.Lock()
	defer s.mu.Unlock()

	caps, known := s.capsLocked()
	if !known || len(caps.ExceptionBreakpointFilters) == 0 {
		return nil
	}

	var modes []api.ExceptionMode

	for _, m := range []api.ExceptionMode{api.ExceptionsAll, api.ExceptionsUncaught} {
		filters := s.filtersFor([]api.ClientExceptionMode{{Mode: m}})
		if len(filters) > 0 && s.checkFiltersLocked(filters) == nil {
			modes = append(modes, m)
		}
	}

	return modes
}
