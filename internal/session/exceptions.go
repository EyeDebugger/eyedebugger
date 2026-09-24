// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"slices"
	"strings"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// Exception description bounds.
const (
	maxExceptionMessage = 1000
	maxStackTraceLines  = 20
	maxStackTraceBytes  = 4000
	maxInnerDepth       = 3
)

// Stop reasons the session looks at.
const (
	reasonException = "exception"
	reasonPause     = "pause"
)

// pauseSignal reports whether stop is a SIGSTOP signal stop, the shape of
// a pause lldb-dap implements with SIGSTOP (Linux): reason exception,
// description "signal SIGSTOP".
func pauseSignal(stop api.StopInfo) bool {
	return stop.Reason == reasonException && stop.Description == "signal SIGSTOP"
}

// Exceptions reports the session's exception modes (p.Mode empty) or sets
// client c's. Each client has its own mode; the adapter stops at the union
// of them. Force sets p.Mode for every client instead.
func (s *Session) Exceptions(ctx context.Context, c api.Client, p api.ExceptionsParams) (api.ExceptionsResult, error) {
	if p.Mode == "" {
		if p.Force {
			return api.ExceptionsResult{}, api.NewError(api.CodeInvalidRequest, "--force needs a mode", "e.g. 'eyedbg bp exceptions none --force'")
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		return s.exceptionsResultLocked(), nil
	}

	mode, err := api.ParseExceptionMode(string(p.Mode))
	if err != nil {
		return api.ExceptionsResult{}, err
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.mu.Lock()
	if s.state == api.StateExited {
		s.mu.Unlock()

		return api.ExceptionsResult{}, stateError(s.ID, s.state, "setting exception stops needs a live session")
	}

	modes := withMode(s.excModes, c.ID, mode, p.Force)
	filters := s.filtersFor(modes)

	if err := s.checkFiltersLocked(filters); err != nil {
		s.mu.Unlock()

		return api.ExceptionsResult{}, err
	}
	s.mu.Unlock()

	if err := s.sendFilters(ctx, filters); err != nil {
		return api.ExceptionsResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.excModes = modes

	e := api.Event{Kind: api.EventExceptions, Client: c.ID, Action: string(mode)}
	if p.Force {
		e.Reason = "force"
	}

	s.log.append(e)

	return s.exceptionsResultLocked(), nil
}

// withMode returns modes with client's mode set (none: removed), keeping
// the order modes were first set; force replaces every client's.
func withMode(modes []api.ClientExceptionMode, client string, mode api.ExceptionMode, force bool) []api.ClientExceptionMode {
	if force {
		if mode == api.ExceptionsNone {
			return nil
		}

		return []api.ClientExceptionMode{{Client: client, Mode: mode}}
	}

	out := make([]api.ClientExceptionMode, 0, len(modes)+1)
	found := false

	for _, m := range modes {
		if m.Client != client {
			out = append(out, m)

			continue
		}

		found = true

		if mode != api.ExceptionsNone {
			out = append(out, api.ClientExceptionMode{Client: client, Mode: mode})
		}
	}

	if !found && mode != api.ExceptionsNone {
		out = append(out, api.ClientExceptionMode{Client: client, Mode: mode})
	}

	return out
}

// filtersFor returns the adapter filters the union of modes turns on, in
// order, never nil.
func (s *Session) filtersFor(modes []api.ClientExceptionMode) []string {
	filters := []string{}

	for _, m := range modes {
		ids, ok := s.excFilters[m.Mode]
		if !ok {
			ids = []string{string(m.Mode)}
		}

		for _, id := range ids {
			if !slices.Contains(filters, id) {
				filters = append(filters, id)
			}
		}
	}

	return filters
}

func (s *Session) exceptionsResultLocked() api.ExceptionsResult {
	modes := slices.Clone(s.excModes)
	if modes == nil {
		modes = []api.ClientExceptionMode{}
	}

	return api.ExceptionsResult{SessionID: s.ID, Modes: modes, Filters: s.filtersFor(s.excModes)}
}

// sendFilters sends the exception filters, once the adapter is initialized
// (until then configure sends them). Call with syncMu held.
func (s *Session) sendFilters(ctx context.Context, filters []string) error {
	select {
	case <-s.initialized:
	default:
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if _, err := s.client.Do(ctx, &godap.SetExceptionBreakpointsRequest{
		Request:   godap.Request{Command: "setExceptionBreakpoints"},
		Arguments: godap.SetExceptionBreakpointsArguments{Filters: filters},
	}); err != nil {
		return adapterErr(err)
	}

	return nil
}

// configureExceptions sends the clients' exception filters during start-up.
func (s *Session) configureExceptions(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.mu.Lock()
	filters := s.filtersFor(s.excModes)
	err := s.checkFiltersLocked(filters)
	s.mu.Unlock()

	if err != nil {
		return err
	}

	return s.sendFilters(ctx, filters)
}

// exceptionInfo describes the exception of stop number stops on thread, if
// the adapter can tell (nil otherwise). It is fetched once per stop.
func (s *Session) exceptionInfo(ctx context.Context, stops, thread int) *api.ExceptionInfo {
	s.mu.Lock()
	caps, _ := s.capsLocked()

	if s.excInfoStops == stops && s.excInfo != nil {
		info := *s.excInfo
		s.mu.Unlock()

		return &info
	}
	s.mu.Unlock()

	if !caps.SupportsExceptionInfoRequest {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.ExceptionInfoResponse](ctx, s.client, &godap.ExceptionInfoRequest{
		Request:   godap.Request{Command: "exceptionInfo"},
		Arguments: godap.ExceptionInfoArguments{ThreadId: thread},
	})
	if err != nil {
		return nil
	}

	info := convertException(resp.Body)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stops == stops {
		s.excInfo, s.excInfoStops = &info, stops
	}

	return &info
}

// convertException converts an exceptionInfo answer, cutting long texts.
func convertException(body godap.ExceptionInfoResponseBody) api.ExceptionInfo {
	info := api.ExceptionInfo{ID: body.ExceptionId, Description: body.Description, BreakMode: string(body.BreakMode)}
	info.Description, info.Truncated = cut(info.Description, maxExceptionMessage)

	if body.Details != nil {
		d := convertDetails(*body.Details, maxInnerDepth)
		info.Type, info.Message, info.StackTrace, info.Inner = d.Type, d.Message, d.StackTrace, d.Inner
		info.Truncated = info.Truncated || d.Truncated
	}

	return info
}

func convertDetails(d godap.ExceptionDetails, depth int) api.ExceptionInfo {
	info := api.ExceptionInfo{Type: d.FullTypeName}
	if info.Type == "" {
		info.Type = d.TypeName
	}

	var cutMsg, cutStack bool

	info.Message, cutMsg = cut(d.Message, maxExceptionMessage)
	info.StackTrace, cutStack = cutStackTrace(d.StackTrace)
	info.Truncated = cutMsg || cutStack

	if depth > 0 {
		for _, inner := range d.InnerException {
			in := convertDetails(inner, depth-1)
			in.StackTrace = ""
			info.Inner = append(info.Inner, in)
			info.Truncated = info.Truncated || in.Truncated
		}
	} else if len(d.InnerException) > 0 {
		info.Truncated = true
	}

	return info
}

func cut(s string, n int) (string, bool) {
	c := present.CutText(s, n, false)

	return c, len(c) < len(s)
}

// cutStackTrace keeps the first lines of a stack trace.
func cutStackTrace(st string) (string, bool) {
	lines := strings.Split(strings.TrimRight(st, "\r\n"), "\n")
	cutLines := len(lines) > maxStackTraceLines

	if cutLines {
		lines = lines[:maxStackTraceLines]
	}

	out, cutBytes := cut(strings.Join(lines, "\n"), maxStackTraceBytes)

	return out, cutLines || cutBytes
}
