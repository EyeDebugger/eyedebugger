// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// pathTranslator rewrites the source paths of a session with a path map at
// its DAP client (docs/adr/0020, D4): host to remote in the requests it
// sends, remote to host in the responses and events it receives. A path
// that doesn't map is left as it is; Source.Name is never touched. Every
// go-dap message type that holds a Source is handled (a test enumerates
// them), plus the session's own setBreakpointsRequest.
//
// It runs on the client's hot paths: pure string work, no locks, no I/O.
type pathTranslator struct{ m *PathMap }

// translator returns the session's translator, or nil (a nil interface,
// never a typed nil) for a session without a path map.
func (s *Session) translator() dap.Translator {
	if s.pathMap == nil {
		return nil
	}

	return pathTranslator{m: s.pathMap}
}

// Outgoing maps the host paths of a request to the debuggee's.
func (t pathTranslator) Outgoing(req godap.RequestMessage) {
	switch r := req.(type) {
	case *setBreakpointsRequest:
		t.out(&r.Arguments.Source)
	case *godap.SetBreakpointsRequest:
		t.out(&r.Arguments.Source)
	case *godap.BreakpointLocationsRequest:
		if r.Arguments != nil { // optional in go-dap
			t.out(&r.Arguments.Source)
		}
	case *godap.GotoTargetsRequest:
		t.out(&r.Arguments.Source)
	case *godap.SourceRequest:
		t.out(r.Arguments.Source)
	}
}

// Incoming maps the debuggee's paths in a response or event to the host's.
func (t pathTranslator) Incoming(msg godap.Message) {
	if bps, ok := breakpointsOf(msg); ok {
		t.breakpoints(bps)

		return
	}

	switch m := msg.(type) {
	case *godap.StackTraceResponse:
		for i := range m.Body.StackFrames {
			t.in(m.Body.StackFrames[i].Source)
		}
	case *godap.ScopesResponse:
		for i := range m.Body.Scopes {
			t.in(m.Body.Scopes[i].Source)
		}
	case *godap.BreakpointEvent:
		t.in(m.Body.Breakpoint.Source)
	case *godap.LoadedSourcesResponse:
		for i := range m.Body.Sources {
			t.in(&m.Body.Sources[i])
		}
	case *godap.LoadedSourceEvent:
		t.in(&m.Body.Source)
	case *godap.OutputEvent:
		t.in(m.Body.Source)
	case *godap.DisassembleResponse:
		for i := range m.Body.Instructions {
			t.in(m.Body.Instructions[i].Location)
		}
	}
}

// breakpointsOf returns the breakpoints of a set…Breakpoints response.
func breakpointsOf(msg godap.Message) ([]godap.Breakpoint, bool) {
	switch m := msg.(type) {
	case *godap.SetBreakpointsResponse:
		return m.Body.Breakpoints, true
	case *godap.SetFunctionBreakpointsResponse:
		return m.Body.Breakpoints, true
	case *godap.SetExceptionBreakpointsResponse:
		return m.Body.Breakpoints, true
	case *godap.SetDataBreakpointsResponse:
		return m.Body.Breakpoints, true
	case *godap.SetInstructionBreakpointsResponse:
		return m.Body.Breakpoints, true
	default:
		return nil, false
	}
}

func (t pathTranslator) breakpoints(bps []godap.Breakpoint) {
	for i := range bps {
		t.in(bps[i].Source)
	}
}

// out maps src's path, and its nested sources', to the debuggee's.
func (t pathTranslator) out(src *godap.Source) {
	walkSource(src, t.m.ToRemote)
}

// in maps src's path, and its nested sources', to the host's.
func (t pathTranslator) in(src *godap.Source) {
	walkSource(src, t.m.ToLocal)
}

// walkSource rewrites the paths of src and of the sources nested in it
// that conv maps; nil does nothing. Nesting is bounded by the decoder's.
func walkSource(src *godap.Source, conv func(string) (string, bool)) {
	if src == nil {
		return
	}

	if src.Path != "" {
		if p, ok := conv(src.Path); ok {
			src.Path = p
		}
	}

	for i := range src.Sources {
		walkSource(&src.Sources[i], conv)
	}
}
