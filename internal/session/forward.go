// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"encoding/json"
	"fmt"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// forwardClass is how [Session.Forward] treats a request.
type forwardClass int

const (
	// classRefused requests are never sent to the adapter.
	classRefused forwardClass = iota
	// classRead requests are sent as they are.
	classRead
	// classStoppedRead requests need a stopped program. completions also
	// needs a 1-based column: without one, an adapter may run the raw
	// text (debugpy does).
	classStoppedRead
	// classGuardedRead is evaluate in a read context: a stopped program,
	// and the driver's side-effect check first.
	classGuardedRead
	// classExecution requests may change the program: they are execution
	// requests of the client's (lease, exec event), like eval with side
	// effects and set.
	classExecution
)

// forwardPolicy is the passthrough policy (ADR 0012): the class of every
// request an editor may send through the DAP facade that the facade doesn't
// handle itself, and for evaluate, the context to send. Anything not listed
// is refused, including every lifecycle, execution and breakpoint request
// (the facade handles those through their own methods).
func forwardPolicy(req godap.RequestMessage) (class forwardClass, evalContext string) {
	switch r := req.(type) {
	case *godap.ThreadsRequest, *godap.SourceRequest, *godap.LoadedSourcesRequest, *godap.ModulesRequest,
		*godap.BreakpointLocationsRequest, *godap.ReadMemoryRequest, *godap.DisassembleRequest:
		return classRead, ""
	case *godap.StackTraceRequest, *godap.ScopesRequest, *godap.VariablesRequest, *godap.ExceptionInfoRequest,
		*godap.CompletionsRequest:
		return classStoppedRead, ""
	case *godap.EvaluateRequest:
		switch c := r.Arguments.Context; c {
		case contextWatch, "hover", "variables":
			return classGuardedRead, c
		case "clipboard": // sent as watch: adapters may not truncate a clipboard value
			return classGuardedRead, contextWatch
		case contextRepl, "": // no context: the adapter may treat it as repl
			return classExecution, contextRepl
		default:
			return classRefused, ""
		}
	case *godap.SetVariableRequest, *godap.SetExpressionRequest:
		return classExecution, ""
	default:
		return classRefused, ""
	}
}

// ForwardsAsExecution reports whether [Session.Forward] treats req as an
// execution request (it takes the control lease and logs an exec event).
func ForwardsAsExecution(req godap.RequestMessage) bool {
	class, _ := forwardPolicy(req)

	return class == classExecution
}

// Forward sends an editor's request to the adapter for client c, as the
// passthrough policy (forwardPolicy) allows, and returns the adapter's
// response. What reaches the adapter is re-encoded from req (fields go-dap
// doesn't model are dropped), never the editor's bytes. evaluate goes with
// the context the policy decided on. Refused requests are INVALID_REQUEST,
// and requests whose capability the adapter didn't declare
// UNSUPPORTED_BY_ADAPTER; neither is sent.
func (s *Session) Forward(ctx context.Context, c api.Client, req godap.RequestMessage) (godap.ResponseMessage, error) {
	class, evalContext := forwardPolicy(req)
	if class == classRefused {
		return nil, refusedRequest(req)
	}

	s.mu.Lock()
	caps, _ := s.capsLocked()
	s.mu.Unlock()

	if !declares(caps, req) {
		return nil, api.NewError(api.CodeUnsupported,
			"the "+s.Lang+" debug adapter doesn't support "+req.GetRequest().Command, "its capabilities (initialize's response) don't declare it")
	}

	out, err := outgoing(req, evalContext)
	if err != nil {
		return nil, err
	}

	switch class {
	case classRead:
		return s.sendRequest(ctx, out)
	case classStoppedRead:
		if comp, ok := out.(*godap.CompletionsRequest); ok && comp.Arguments.Column < 1 {
			return nil, api.NewError(api.CodeInvalidRequest, "completions needs a 1-based column",
				"DAP columns start at 1 (columnsStartAt1); an omitted column arrives as 0, which some adapters (debugpy) treat as license to run the raw text")
		}

		if _, err := s.requireStopped(); err != nil {
			return nil, err
		}

		return s.sendRequest(ctx, out)
	case classGuardedRead:
		return s.forwardGuarded(ctx, out)
	case classExecution:
		return s.forwardExecution(ctx, c, out)
	case classRefused:
	}

	return nil, refusedRequest(req)
}

// declares reports whether the adapter declared the capability req needs,
// if any. A request an adapter doesn't support may go unanswered, and some
// adapters (debugpy) answer nothing more after one: such requests are
// never sent.
func declares(caps godap.Capabilities, req godap.RequestMessage) bool {
	switch req.(type) {
	case *godap.LoadedSourcesRequest:
		return caps.SupportsLoadedSourcesRequest
	case *godap.ModulesRequest:
		return caps.SupportsModulesRequest
	case *godap.BreakpointLocationsRequest:
		return caps.SupportsBreakpointLocationsRequest
	case *godap.ReadMemoryRequest:
		return caps.SupportsReadMemoryRequest
	case *godap.DisassembleRequest:
		return caps.SupportsDisassembleRequest
	case *godap.ExceptionInfoRequest:
		return caps.SupportsExceptionInfoRequest
	case *godap.CompletionsRequest:
		return caps.SupportsCompletionsRequest
	default:
		return true
	}
}

// refusedRequest is the error for a request the policy refuses.
func refusedRequest(req godap.RequestMessage) error {
	r := req.GetRequest()
	if e, ok := req.(*godap.EvaluateRequest); ok {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("evaluate context %q is not supported", e.Arguments.Context),
			"use watch, hover, variables, clipboard or repl")
	}

	return api.NewError(api.CodeInvalidRequest, "eyedbg's DAP facade does not support "+r.Command, "")
}

// outgoing returns a copy of req to send to the adapter, re-encoded from
// its go-dap value (the adapter client assigns its own seq), with
// evalContext as an evaluate request's context when set.
func outgoing(req godap.RequestMessage, evalContext string) (godap.RequestMessage, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", req.GetRequest().Command, err)
	}

	msg, err := godap.DecodeProtocolMessage(raw)
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", req.GetRequest().Command, err)
	}

	out, ok := msg.(godap.RequestMessage)
	if !ok {
		return nil, fmt.Errorf("copy %s: got %T", req.GetRequest().Command, msg)
	}

	if e, ok := out.(*godap.EvaluateRequest); ok && evalContext != "" {
		e.Arguments.Context = evalContext
	}

	return out, nil
}

// forwardGuarded sends evaluate in a read context: it must name a frame,
// the program must be stopped, and an expression the driver sees changing
// it is SIDE_EFFECTS. Without a frame an adapter may run the text as
// statements whatever the context (debugpy does), which the guard can't
// judge; frameId 0 is the same, as go-dap drops it when re-encoding.
func (s *Session) forwardGuarded(ctx context.Context, req godap.RequestMessage) (godap.ResponseMessage, error) {
	e, ok := req.(*godap.EvaluateRequest)
	if !ok {
		return nil, refusedRequest(req)
	}

	if e.Arguments.FrameId == 0 {
		return nil, api.NewError(api.CodeInvalidRequest, "evaluate in a read context (watch, hover, variables, clipboard) needs a frameId",
			"pass a frame from stackTrace, or evaluate in the debug console (repl): that takes the control lease")
	}

	if _, err := s.requireStopped(); err != nil {
		return nil, err
	}

	if reason, found := s.sideEffectsOf(e.Arguments.Expression); found {
		return nil, api.NewError(api.CodeSideEffects, "evaluating would "+reason+", which can change the program",
			"evaluate it in the debug console (repl): that takes the control lease")
	}

	return s.sendRequest(ctx, req)
}

// forwardExecution sends evaluate (repl), setVariable or setExpression as
// an execution request of c's: under execMu, the state and the lease first,
// then the exec event (the expression or variable name, never a value) and
// the send; the current stop's captured locals become stale.
func (s *Session) forwardExecution(ctx context.Context, c api.Client, req godap.RequestMessage) (godap.ResponseMessage, error) {
	x := execRequest{client: c}

	s.mu.Lock()
	caps, _ := s.capsLocked()
	s.mu.Unlock()

	switch r := req.(type) {
	case *godap.EvaluateRequest:
		x.kind, x.text = ExecEval, r.Arguments.Expression
	case *godap.SetVariableRequest:
		if !caps.SupportsSetVariable {
			return nil, s.unsupported("set variables")
		}

		x.kind, x.text = ExecSet, r.Arguments.Name
	case *godap.SetExpressionRequest:
		if !caps.SupportsSetExpression {
			return nil, s.unsupported("set expressions")
		}

		x.kind, x.text = ExecSet, r.Arguments.Expression
	default:
		return nil, refusedRequest(req)
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	if _, _, err := s.admit(x); err != nil {
		return nil, err
	}

	defer s.markStale()

	return s.sendRequest(ctx, req)
}

// sendRequest sends req to the adapter and returns its response; a failed
// response is ADAPTER_ERROR.
func (s *Session) sendRequest(ctx context.Context, req godap.RequestMessage) (godap.ResponseMessage, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()

	if client == nil {
		return nil, api.NewError(api.CodeSessionExited, "the session has no debug adapter", "see 'eyedbg status'")
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	msg, err := client.Do(ctx, req)
	if err != nil {
		return nil, adapterErr(err)
	}

	resp, ok := msg.(godap.ResponseMessage)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected response type %T", req.GetRequest().Command, msg)
	}

	return resp, nil
}
