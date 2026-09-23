// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// Execution kinds of eval with side effects and set: both change the
// program, so they are serialized and leased like execution requests.
const (
	execEval = "eval"
	execSet  = "set"
)

// Evaluation contexts: watch for reads, repl for what may change the
// program.
const (
	contextWatch = "watch"
	contextRepl  = "repl"
)

// sideEffectsHint says how to evaluate what the check refused.
const sideEffectsHint = "to evaluate an expression that calls methods, use 'eyedbg eval EXPR --allow-side-effects' (it takes the control lease)"

// Eval evaluates p.Expression in frame p.Frame of the stopped program for
// client c. Without p.AllowSideEffects an expression the driver sees
// changing the program is SIDE_EFFECTS; with it, the evaluation is an
// execution request of c's (lease, exec event) in the repl context.
func (s *Session) Eval(ctx context.Context, c api.Client, p api.EvalParams) (api.EvalResult, error) {
	if !p.AllowSideEffects {
		if reason, found := s.sideEffectsOf(p.Expression); found {
			return api.EvalResult{}, api.NewError(api.CodeSideEffects,
				"eval would "+reason+", which can change the program", "add --allow-side-effects (it takes the control lease)")
		}

		return s.evaluate(ctx, p, contextWatch)
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	if _, _, err := s.admit(execRequest{client: c, kind: execEval, text: p.Expression}); err != nil {
		return api.EvalResult{}, err
	}

	defer s.markStale()

	return s.evaluate(ctx, p, contextRepl)
}

// sideEffectsOf runs the driver's side-effect check on expr.
func (s *Session) sideEffectsOf(expr string) (string, bool) {
	s.mu.Lock()
	check := s.sideEffects
	s.mu.Unlock()

	if check == nil {
		return "", false
	}

	return check(expr)
}

// evaluate sends evaluate in evalContext and expands the result's members
// p.Depth-1 levels, cut to p.Budget tokens.
func (s *Session) evaluate(ctx context.Context, p api.EvalParams, evalContext string) (api.EvalResult, error) {
	fid, err := s.frameID(ctx, p.Frame)
	if err != nil {
		return api.EvalResult{}, err
	}

	body, err := s.evalBody(ctx, fid, p.Expression, evalContext)
	if err != nil {
		return api.EvalResult{}, err
	}

	ref := body.VariablesReference
	res := api.EvalResult{Expression: p.Expression, Value: truncate(body.Result), Type: body.Type, HasChildren: ref > 0}

	if p.Depth > 1 && ref > 0 {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()

		children, more, err := s.variables(ctx, ref, p.Depth-1)
		if err != nil {
			return api.EvalResult{}, err
		}

		var omitted int

		res.Children, omitted, res.Truncated = present.NewFitter(p.Budget).Fit(children)
		res.More = more + omitted
	}

	return res, nil
}

// Set assigns p.Value (an expression) to p.Variable (a name or member path)
// in frame p.Frame, for client c: an execution request (lease, exec event).
// It uses the adapter's setExpression, else setVariable on the variable's
// parent, else it is UNSUPPORTED_BY_ADAPTER.
func (s *Session) Set(ctx context.Context, c api.Client, p api.SetParams) (api.SetResult, error) {
	segs, err := splitPath(p.Variable)
	if err != nil {
		return api.SetResult{}, err
	}

	s.mu.Lock()
	caps, _ := s.capsLocked()
	s.mu.Unlock()

	if !caps.SupportsSetExpression && !caps.SupportsSetVariable {
		return api.SetResult{}, s.unsupported("set variables")
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	if _, _, err := s.admit(execRequest{client: c, kind: execSet, text: p.Variable}); err != nil {
		return api.SetResult{}, err
	}

	defer s.markStale()

	fid, err := s.frameID(ctx, p.Frame)
	if err != nil {
		return api.SetResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if caps.SupportsSetExpression {
		resp, err := dap.Call[*godap.SetExpressionResponse](ctx, s.client, &godap.SetExpressionRequest{
			Request:   godap.Request{Command: "setExpression"},
			Arguments: godap.SetExpressionArguments{Expression: p.Variable, Value: p.Value, FrameId: fid},
		})
		if err != nil {
			return api.SetResult{}, adapterErr(err)
		}

		return api.SetResult{Variable: p.Variable, Value: truncate(resp.Body.Value), Type: resp.Body.Type}, nil
	}

	return s.setVariable(ctx, fid, segs, p)
}

// setVariable sets the last of segs through setVariable on its parent: the
// first cheap scope holding a single name, else the member path's parent.
func (s *Session) setVariable(ctx context.Context, fid int, segs []string, p api.SetParams) (api.SetResult, error) {
	scopes, err := s.scopes(ctx, fid)
	if err != nil {
		return api.SetResult{}, err
	}

	var (
		candidates []godap.Variable
		scopeOf    = map[string]int{}
	)

	for _, sc := range scopes {
		if sc.Expensive {
			continue
		}

		vars, err := s.children(ctx, sc.VariablesReference)
		if err != nil {
			return api.SetResult{}, err
		}

		for _, v := range vars {
			if _, ok := scopeOf[v.Name]; !ok {
				scopeOf[v.Name] = sc.VariablesReference
			}
		}

		candidates = append(candidates, vars...)
	}

	parent, ok := scopeOf[segs[0]]
	if !ok {
		return api.SetResult{}, noMember(nil, segs[0], candidates)
	}

	if len(segs) > 1 {
		v, err := s.walk(ctx, candidates, segs[:len(segs)-1])
		if err != nil {
			return api.SetResult{}, err
		}

		if v.VariablesReference == 0 {
			return api.SetResult{}, api.NewError(api.CodeInvalidRequest, joinPath(segs[:len(segs)-1])+" has no members", "")
		}

		parent = v.VariablesReference
	}

	resp, err := dap.Call[*godap.SetVariableResponse](ctx, s.client, &godap.SetVariableRequest{
		Request:   godap.Request{Command: "setVariable"},
		Arguments: godap.SetVariableArguments{VariablesReference: parent, Name: segs[len(segs)-1], Value: p.Value},
	})
	if err != nil {
		return api.SetResult{}, adapterErr(err)
	}

	return api.SetResult{Variable: p.Variable, Value: truncate(resp.Body.Value), Type: resp.Body.Type}, nil
}

// markStale makes the next capture refetch the current stop's locals
// (something may have changed them) without moving the previous stop's.
// It waits for a capture in flight, so that one can't store values from
// before the change as fresh.
func (s *Session) markStale() {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.curStale = true
}

// withHint returns err (an *api.Error) with hint instead of its own.
func withHint(err error, hint string) error {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return api.NewError(e.Code, e.Message, hint)
	}

	return err
}
