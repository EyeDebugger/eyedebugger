// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// Inspection limits; token budgets (docs/DESIGN.md §5) cut further.
const (
	maxChildren    = 50
	maxValueLength = 200
)

// Vars returns the scopes of frame (an index into the stopped thread's
// stack) with their variables, expanded depth levels deep (1 = no children)
// and cut to budget tokens (0: no cap).
func (s *Session) Vars(ctx context.Context, frameIndex, depth, budget int) ([]api.Scope, error) {
	fid, err := s.frameID(ctx, frameIndex)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	scopes, err := s.scopes(ctx, fid)
	if err != nil {
		return nil, err
	}

	fitter := present.NewFitter(budget)
	out := make([]api.Scope, 0, len(scopes))

	for _, sc := range scopes {
		scope := api.Scope{Name: sc.Name, Expensive: sc.Expensive}

		if !sc.Expensive {
			vars, more, err := s.variables(ctx, sc.VariablesReference, depth)
			if err != nil {
				return nil, err
			}

			var omitted int
			scope.Vars, omitted, scope.Truncated = fitter.Fit(vars)
			scope.More = more + omitted
		}

		out = append(out, scope)
	}

	return out, nil
}

// Expand returns the variable at path (e.g. order.Items[0]) in frame, with
// its members expanded depth levels deep, cut to budget tokens.
func (s *Session) Expand(ctx context.Context, frameIndex int, path string, depth, budget int) ([]api.Scope, error) {
	segs, err := splitPath(path)
	if err != nil {
		return nil, err
	}

	fid, err := s.frameID(ctx, frameIndex)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	scopes, err := s.scopes(ctx, fid)
	if err != nil {
		return nil, err
	}

	// The first segment may be in any (cheap) scope.
	var candidates []godap.Variable

	for _, sc := range scopes {
		if sc.Expensive {
			continue
		}

		vars, err := s.children(ctx, sc.VariablesReference)
		if err != nil {
			return nil, err
		}

		candidates = append(candidates, vars...)
	}

	target, err := s.walk(ctx, candidates, segs)
	if err != nil {
		if target, err = s.evaluatePath(ctx, fid, path, err); err != nil {
			return nil, err
		}
	}

	v := api.Var{Name: target.Name, Type: target.Type, Value: truncate(target.Value), HasChildren: target.VariablesReference > 0}
	if v.HasChildren {
		if v.Children, v.More, err = s.variables(ctx, target.VariablesReference, depth); err != nil {
			return nil, err
		}
	}

	scope := api.Scope{Name: path}

	var omitted int
	scope.Vars, omitted, scope.Truncated = present.NewFitter(budget).Fit([]api.Var{v})
	scope.More = omitted

	return []api.Scope{scope}, nil
}

// evaluatePath evaluates path, which is not a chain of members as the
// adapter shows them (e.g. an index into a List, which netcoredbg shows as
// raw fields), unless that would visibly change the program. walkErr is
// the error returned when it can't.
func (s *Session) evaluatePath(ctx context.Context, fid int, path string, walkErr error) (godap.Variable, error) {
	if _, found := s.sideEffectsOf(path); found {
		return godap.Variable{}, withHint(walkErr, sideEffectsHint)
	}

	body, err := s.evalBody(ctx, fid, path, contextWatch)
	if err != nil {
		return godap.Variable{}, walkErr
	}

	return godap.Variable{Name: path, Value: body.Result, Type: body.Type, VariablesReference: body.VariablesReference}, nil
}

// walk follows segs from candidates (the first segment's siblings) down
// through members.
func (s *Session) walk(ctx context.Context, candidates []godap.Variable, segs []string) (godap.Variable, error) {
	var cur godap.Variable

	for i, seg := range segs {
		found := false

		for _, c := range candidates {
			if c.Name == seg {
				cur, found = c, true

				break
			}
		}

		if !found {
			return godap.Variable{}, noMember(segs[:i], seg, candidates)
		}

		if i == len(segs)-1 {
			break
		}

		if cur.VariablesReference == 0 {
			return godap.Variable{}, api.NewError(api.CodeInvalidRequest,
				joinPath(segs[:i+1])+" has no members", "")
		}

		var err error
		if candidates, err = s.children(ctx, cur.VariablesReference); err != nil {
			return godap.Variable{}, err
		}
	}

	return cur, nil
}

func noMember(parent []string, seg string, candidates []godap.Variable) error {
	names := make([]string, 0, min(len(candidates), maxChildren))
	for _, c := range candidates[:min(len(candidates), maxChildren)] {
		names = append(names, c.Name)
	}

	where := "the frame's variables"
	if len(parent) > 0 {
		where = joinPath(parent)
	}

	return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("no %s in %s", seg, where),
		"names there: "+strings.Join(names, ", "))
}

// splitPath splits a.b[0].c into a, b, [0], c: member names as the adapter
// shows them (indexes are members named [N]).
func splitPath(path string) ([]string, error) {
	var segs []string

	rest := path
	for rest != "" {
		switch {
		case rest[0] == '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, api.NewError(api.CodeInvalidRequest, "unclosed [ in "+path, "write paths like order.Items[0].Name")
			}

			segs = append(segs, rest[:end+1])
			rest = rest[end+1:]
		case rest[0] == '.' && len(segs) > 0:
			rest = rest[1:]
		default:
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}

			if end == 0 {
				return nil, api.NewError(api.CodeInvalidRequest, "empty name in "+path, "write paths like order.Items[0].Name")
			}

			segs = append(segs, rest[:end])
			rest = rest[end:]
		}
	}

	if len(segs) == 0 {
		return nil, api.NewError(api.CodeInvalidRequest, "empty variable path", "write paths like order.Items[0].Name")
	}

	return segs, nil
}

func joinPath(segs []string) string {
	var b strings.Builder

	for i, seg := range segs {
		if i > 0 && !strings.HasPrefix(seg, "[") {
			b.WriteByte('.')
		}

		b.WriteString(seg)
	}

	return b.String()
}

// Changes returns frame 0's locals that changed since the previous stop,
// cut to budget tokens.
func (s *Session) Changes(ctx context.Context, budget int) ([]api.Scope, error) {
	cur, prev, err := s.capture(ctx)
	if err != nil {
		return nil, err
	}

	ch := changes(cur, prev)

	scope := api.Scope{Name: "Changed since the previous stop"}
	if ch.NewFrame {
		scope.Name = "Locals (first stop in this function: all new)"
	}

	scope.Vars, scope.More, scope.Truncated = present.NewFitter(budget).Fit(ch.Vars)

	return []api.Scope{scope}, nil
}

// frameLocals returns the variables of frame fid's local scopes (see
// localScopes), one level deep.
func (s *Session) frameLocals(ctx context.Context, fid int) ([]api.Var, int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	scopes, err := s.scopes(ctx, fid)
	if err != nil {
		return nil, 0, err
	}

	var (
		all  []api.Var
		more int
	)

	for _, sc := range localScopes(scopes) {
		vars, m, err := s.variables(ctx, sc.VariablesReference, 1)
		if err != nil {
			return nil, 0, err
		}

		all = append(all, vars...)
		more += m
	}

	return all, more, nil
}

// localScopes picks the scopes a stop capture reads: those the adapter
// marks with presentationHint "locals" when it marks any (an adapter that
// also returns globals), else every cheap one.
func localScopes(scopes []godap.Scope) []godap.Scope {
	var marked, cheap []godap.Scope

	for _, sc := range scopes {
		if sc.PresentationHint == "locals" {
			marked = append(marked, sc)
		}

		if !sc.Expensive {
			cheap = append(cheap, sc)
		}
	}

	if len(marked) > 0 {
		return marked
	}

	return cheap
}

func (s *Session) scopes(ctx context.Context, fid int) ([]godap.Scope, error) {
	resp, err := dap.Call[*godap.ScopesResponse](ctx, s.client, &godap.ScopesRequest{
		Request:   godap.Request{Command: "scopes"},
		Arguments: godap.ScopesArguments{FrameId: fid},
	})
	if err != nil {
		return nil, adapterErr(err)
	}

	return resp.Body.Scopes, nil
}

// children fetches the members of ref as the adapter returns them.
func (s *Session) children(ctx context.Context, ref int) ([]godap.Variable, error) {
	resp, err := dap.Call[*godap.VariablesResponse](ctx, s.client, &godap.VariablesRequest{
		Request:   godap.Request{Command: "variables"},
		Arguments: godap.VariablesArguments{VariablesReference: ref},
	})
	if err != nil {
		return nil, adapterErr(err)
	}

	return resp.Body.Variables, nil
}

// variables fetches the children of ref, recursing depth-1 more levels.
func (s *Session) variables(ctx context.Context, ref, depth int) ([]api.Var, int, error) {
	vars, err := s.children(ctx, ref)
	if err != nil {
		return nil, 0, err
	}

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

// Output returns the program's output chunks with Seq > since, at most tail
// of them (0: all), within present.MaxResultBytes: the oldest ones, or the
// newest with a tail. More counts the chunks the size cap left out.
func (s *Session) Output(since, tail int) api.OutputResult {
	lines := s.log.outputSince(since)

	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	kept, more := present.CapOutput(lines, present.MaxResultBytes, tail > 0)
	if kept == nil {
		kept = []api.OutputLine{}
	}

	return api.OutputResult{Lines: kept, More: more}
}

// Events returns the session's events that q asks for, waiting up to wait
// (0: not at all) for one to exist. Output text is cut and the result fits
// q.Budget tokens and present.MaxResultBytes.
func (s *Session) Events(ctx context.Context, q api.EventsParams, wait time.Duration) (api.EventsResult, error) {
	for _, k := range q.Kinds {
		if !slices.Contains(api.EventKinds(), k) {
			return api.EventsResult{}, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("unknown event kind %q", k),
				"kinds: "+api.EventKindNames())
		}
	}

	var res api.EventsResult

	if wait > 0 {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()

		res = s.log.wait(ctx, q)
	} else {
		res = s.log.query(q)
	}

	var omitted int

	res.Events, omitted = present.ShapeEvents(res.Events, q.Budget, q.Newest)
	res.More += omitted

	return res, nil
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
