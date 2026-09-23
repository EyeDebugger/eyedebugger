// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"path/filepath"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// AddBreakpoint adds a line breakpoint and, once the session is configured,
// sends it to the adapter. The result says whether the adapter verified it
// and on which line it actually landed.
func (s *Session) AddBreakpoint(ctx context.Context, spec api.BreakpointSpec) (api.Breakpoint, error) {
	if !filepath.IsAbs(spec.File) || spec.Line < 1 {
		return api.Breakpoint{}, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("invalid breakpoint %s:%d", spec.File, spec.Line), "use FILE:LINE with a line number from 1")
	}

	s.mu.Lock()
	if s.state == api.StateExited {
		s.mu.Unlock()

		return api.Breakpoint{}, stateError(s.ID, s.state, "adding a breakpoint needs a live session")
	}

	b := s.addBreakpointLocked(spec)
	s.mu.Unlock()

	if err := s.syncBreakpoints(ctx, spec.File); err != nil {
		return api.Breakpoint{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return b.Breakpoint, nil
}

// RemoveBreakpoint removes the breakpoint with id, or every breakpoint if id
// is 0. It returns how many were removed.
func (s *Session) RemoveBreakpoint(ctx context.Context, id int) (int, error) {
	s.mu.Lock()

	removed := 0
	touched := map[string]bool{}

	for file, list := range s.bps {
		kept := list[:0]

		for _, b := range list {
			if id == 0 || b.ID == id {
				removed++
				touched[file] = true

				continue
			}

			kept = append(kept, b)
		}

		s.bps[file] = kept
	}

	s.mu.Unlock()

	if removed == 0 {
		return 0, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("no breakpoint %d", id), "see 'eyedbg bp ls'")
	}

	for file := range touched {
		if err := s.syncBreakpoints(ctx, file); err != nil {
			return removed, err
		}

		s.mu.Lock()
		if len(s.bps[file]) == 0 {
			delete(s.bps, file)
		}
		s.mu.Unlock()
	}

	return removed, nil
}

func (s *Session) addBreakpointLocked(spec api.BreakpointSpec) *breakpoint {
	for _, b := range s.bps[spec.File] {
		if b.RequestedLine == spec.Line {
			return b
		}
	}

	s.nextBP++
	b := &breakpoint{Breakpoint: api.Breakpoint{
		ID: s.nextBP, File: spec.File, RequestedLine: spec.Line, Line: spec.Line,
	}}
	s.bps[spec.File] = append(s.bps[spec.File], b)

	return b
}

// syncBreakpoints sends file's full breakpoint list to the adapter (DAP
// replaces per file) and records what the adapter answered. Before the
// adapter is initialized there is nothing to do: configure sends them.
func (s *Session) syncBreakpoints(ctx context.Context, file string) error {
	select {
	case <-s.initialized:
	default:
		return nil
	}

	s.mu.Lock()
	list := append([]*breakpoint(nil), s.bps[file]...)
	s.mu.Unlock()

	sbps := make([]godap.SourceBreakpoint, len(list))
	for i, b := range list {
		sbps[i] = godap.SourceBreakpoint{Line: b.RequestedLine}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.SetBreakpointsResponse](ctx, s.client, &setBreakpointsRequest{
		Request:   godap.Request{Command: "setBreakpoints"},
		Arguments: setBreakpointsArguments{Source: godap.Source{Path: file, Name: filepath.Base(file)}, Breakpoints: sbps},
	})
	if err != nil {
		return adapterErr(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, got := range resp.Body.Breakpoints {
		if i >= len(list) {
			break
		}

		b := list[i]
		b.adapterID = got.Id
		b.Verified = got.Verified
		b.Message = got.Message

		if got.Line > 0 {
			b.Line = got.Line
		}
	}

	return nil
}

// updateBreakpointLocked applies an adapter's breakpoint event (e.g. a
// pending breakpoint that resolved once the module loaded).
func (s *Session) updateBreakpointLocked(got godap.Breakpoint) {
	for _, list := range s.bps {
		for _, b := range list {
			if b.adapterID != got.Id || got.Id == 0 {
				continue
			}

			b.Verified = got.Verified
			b.Message = got.Message

			if got.Line > 0 {
				b.Line = got.Line
			}
		}
	}
}

// setBreakpointsRequest is godap.SetBreakpointsRequest without omitempty on
// breakpoints: clearing a file's breakpoints must send "breakpoints": [],
// and some adapters (netcoredbg) reject a request without the key.
type setBreakpointsRequest struct {
	godap.Request

	Arguments setBreakpointsArguments `json:"arguments"`
}

type setBreakpointsArguments struct {
	Source      godap.Source             `json:"source"`
	Breakpoints []godap.SourceBreakpoint `json:"breakpoints"`
}
