// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"context"
	"os"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// Inspection limits (docs/DESIGN.md §5 budgets arrive in milestone 3).
const (
	maxChildren    = 50
	maxValueLength = 200
)

// Vars returns the scopes of frame (an index into the stopped thread's
// stack) with their variables, expanded depth levels deep (1 = no children).
func (s *Session) Vars(ctx context.Context, frameIndex, depth int) ([]api.Scope, error) {
	fid, err := s.frameID(ctx, frameIndex)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.ScopesResponse](ctx, s.client, &godap.ScopesRequest{
		Request:   godap.Request{Command: "scopes"},
		Arguments: godap.ScopesArguments{FrameId: fid},
	})
	if err != nil {
		return nil, adapterErr(err)
	}

	scopes := make([]api.Scope, 0, len(resp.Body.Scopes))

	for _, sc := range resp.Body.Scopes {
		scope := api.Scope{Name: sc.Name}

		if !sc.Expensive {
			scope.Vars, scope.More, err = s.variables(ctx, sc.VariablesReference, depth)
			if err != nil {
				return nil, err
			}
		} else {
			scope.Expensive = true
		}

		scopes = append(scopes, scope)
	}

	return scopes, nil
}

// variables fetches the children of ref, recursing depth-1 more levels.
func (s *Session) variables(ctx context.Context, ref, depth int) ([]api.Var, int, error) {
	resp, err := dap.Call[*godap.VariablesResponse](ctx, s.client, &godap.VariablesRequest{
		Request:   godap.Request{Command: "variables"},
		Arguments: godap.VariablesArguments{VariablesReference: ref},
	})
	if err != nil {
		return nil, 0, adapterErr(err)
	}

	vars := resp.Body.Variables
	more := 0

	if len(vars) > maxChildren {
		more = len(vars) - maxChildren
		vars = vars[:maxChildren]
	}

	out := make([]api.Var, 0, len(vars))

	for _, v := range vars {
		av := api.Var{Name: v.Name, Type: v.Type, Value: truncate(v.Value), HasChildren: v.VariablesReference > 0}

		if av.HasChildren && depth > 1 {
			av.Children, av.More, err = s.variables(ctx, v.VariablesReference, depth-1)
			if err != nil {
				return nil, 0, err
			}
		}

		out = append(out, av)
	}

	return out, more, nil
}

// Eval evaluates expr in frame (an index into the stopped thread's stack).
func (s *Session) Eval(ctx context.Context, expr string, frameIndex int) (api.EvalResult, error) {
	fid, err := s.frameID(ctx, frameIndex)
	if err != nil {
		return api.EvalResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.EvaluateResponse](ctx, s.client, &godap.EvaluateRequest{
		Request:   godap.Request{Command: "evaluate"},
		Arguments: godap.EvaluateArguments{Expression: expr, FrameId: fid, Context: "watch"},
	})
	if err != nil {
		return api.EvalResult{}, adapterErr(err)
	}

	return api.EvalResult{
		Expression: expr, Value: truncate(resp.Body.Result), Type: resp.Body.Type,
		HasChildren: resp.Body.VariablesReference > 0,
	}, nil
}

// Output returns program output lines with Seq > since, at most tail of
// them (0: all).
func (s *Session) Output(since, tail int) []api.OutputLine {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []api.OutputLine

	for _, l := range s.output {
		if l.Seq > since {
			out = append(out, l)
		}
	}

	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}

	return out
}

func truncate(v string) string {
	r := []rune(v)
	if len(r) <= maxValueLength {
		return v
	}

	return string(r[:maxValueLength]) + "…"
}

// readSource returns the lines around line (context lines each side), or
// nil if the file can't be read.
func readSource(path string, line, around int) []api.SourceLine {
	if path == "" || line < 1 {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	if info, err := f.Stat(); err != nil || info.Size() > maxSourceFileSize {
		return nil
	}

	var out []api.SourceLine

	sc := bufio.NewScanner(f)

	for n := 1; sc.Scan(); n++ {
		if n < line-around {
			continue
		}

		if n > line+around {
			break
		}

		out = append(out, api.SourceLine{Line: n, Text: sc.Text(), Current: n == line})
	}

	return out
}
