// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// funcKey is the key of function breakpoints in Session.bps; file keys are
// absolute paths, so it can't clash.
const funcKey = ""

// syncKey sends the breakpoints under key (a file, or funcKey) to the
// adapter.
func (s *Session) syncKey(ctx context.Context, key string) error {
	if key == funcKey {
		return s.syncFunctionBreakpoints(ctx)
	}

	return s.syncBreakpoints(ctx, key)
}

// syncFunctionBreakpoints sends every function breakpoint, one per name, and
// records what the adapter answered, like syncBreakpoints for a file.
func (s *Session) syncFunctionBreakpoints(ctx context.Context) error {
	select {
	case <-s.initialized:
	default:
		return nil
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.mu.Lock()
	slots := slotsFor(s.bps[funcKey])
	s.mu.Unlock()

	fbps := make([]godap.FunctionBreakpoint, len(slots))
	for i, sl := range slots {
		fbps[i] = godap.FunctionBreakpoint{Name: sl.function, Condition: sl.condition}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// fbps is never nil, so no breakpoints encodes as [], not null.
	resp, err := dap.Call[*godap.SetFunctionBreakpointsResponse](ctx, s.client, &godap.SetFunctionBreakpointsRequest{
		Request:   godap.Request{Command: "setFunctionBreakpoints"},
		Arguments: godap.SetFunctionBreakpointsArguments{Breakpoints: fbps},
	})
	if err != nil {
		return adapterErr(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.applySlotsLocked(slots, resp.Body.Breakpoints)

	return nil
}

// applySlotsLocked records the adapter's answer for each slot on every
// breakpoint of that slot.
func (*Session) applySlotsLocked(slots []slot, got []godap.Breakpoint) {
	for i, g := range got {
		if i >= len(slots) {
			break
		}

		note := ""
		if slots[i].note {
			note = sharedLineNote
		}

		for _, b := range slots[i].bps {
			b.adapterID = g.Id
			b.Verified = g.Verified
			b.Message = g.Message
			b.Note = note

			if g.Line > 0 && b.Function == "" {
				b.Line = g.Line
			}

			if g.Source != nil && g.Source.Path != "" {
				b.source = g.Source.Path
			}
		}
	}
}
