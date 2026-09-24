// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"slices"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// callsCheck flags expressions with "(" as method calls.
func callsCheck(expr string) (string, bool) {
	if strings.Contains(expr, "(") {
		return "call a method", true
	}

	return "", false
}

func TestEvalSideEffects(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManagerWith(t, nil, fakeDriver{sideEffects: callsCheck}), agentC, api.StartParams{
		LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true},
	})

	if got := evalValue(t, s, "$context"); got != contextWatch {
		t.Errorf("plain eval context = %q, want watch", got)
	}

	_, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: "Foo()"})
	expectCode(t, err, api.CodeSideEffects)

	// The loophole through vars --expand is closed too.
	_, err = s.Expand(t.Context(), 0, "Foo()", 1, 0)
	if e, ok := err.(*api.Error); !ok || !strings.Contains(e.Hint, "--allow-side-effects") { //nolint:errorlint // Returned unwrapped.
		t.Errorf("expand Foo() err = %v, want the member error with the side-effects hint", err)
	}

	execs := len(eventsOf(s, api.EventExec))

	_, err = s.Eval(t.Context(), humanC, api.EvalParams{Expression: "$context", AllowSideEffects: true})
	expectCode(t, err, api.CodeLeaseHeld)

	_, err = s.Set(t.Context(), humanC, api.SetParams{Variable: "x", Value: "1"})
	expectCode(t, err, api.CodeLeaseHeld)

	if n := len(eventsOf(s, api.EventExec)); n != execs {
		t.Errorf("refused eval/set logged %d exec events", n-execs)
	}

	res, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: "$context", AllowSideEffects: true})
	if err != nil || res.Value != contextRepl {
		t.Fatalf("eval with side effects = %+v, %v; want the repl context", res, err)
	}

	last := eventsOf(s, api.EventExec)
	if e := last[len(last)-1]; e.Action != ExecEval || e.Text != "$context" || e.Client != agentC.ID {
		t.Errorf("exec event = %+v, want agent's eval of $context", e)
	}
}

func TestEvalWithSideEffectsTakesAFreeLease(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	since := s.log.latest()

	if _, err := s.Eval(t.Context(), humanC, api.EvalParams{Expression: "x", AllowSideEffects: true}); err != nil {
		t.Fatal(err)
	}

	got := kindsAndActions(s.log.query(api.EventsParams{Since: since, Kinds: []api.EventKind{api.EventLease, api.EventExec}}).Events)
	if !slices.Equal(got, []string{"lease:auto", "exec:eval"}) {
		t.Errorf("events = %v, want the human auto-taking the lease, then eval", got)
	}
}

func TestEvalDepth(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	res, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: "obj", Depth: 2})
	if err != nil || !res.HasChildren || len(res.Children) != 2 || res.Children[0].Name != "a" || res.Children[1].Value != "2" || res.Truncated {
		t.Fatalf("eval obj --depth 2 = %+v, %v; want a and b", res, err)
	}

	res, err = s.Eval(t.Context(), agentC, api.EvalParams{Expression: "obj", Depth: 2, Budget: 1})
	if err != nil || !res.Truncated || len(res.Children)+res.More != 2 {
		t.Fatalf("eval obj with budget 1 = %+v, %v; want truncated", res, err)
	}

	if res, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: "obj"}); err != nil || len(res.Children) != 0 {
		t.Fatalf("eval obj = %+v, %v; want no children without a depth", res, err)
	}
}

func TestSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		caps *godap.Capabilities
		code api.Code
	}{
		{name: "setExpression"},
		{name: "setVariable", caps: &godap.Capabilities{SupportsConfigurationDoneRequest: true, SupportsSetVariable: true}},
		{name: "neither", caps: &godap.Capabilities{SupportsConfigurationDoneRequest: true}, code: api.CodeUnsupported},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			drv := fakeDriver{opts: daptest.Options{Caps: tt.caps}}
			s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

			// Stop twice, so a changed local diffs against a previous stop.
			expectStopped(t, resume(t, s, agentC, ExecNext), "step", 2)

			if _, err := s.Changes(t.Context(), 0); err != nil {
				t.Fatal(err)
			}

			res, err := s.Set(t.Context(), agentC, api.SetParams{Variable: "x", Value: "5"})
			if tt.code != "" {
				expectCode(t, err, tt.code)

				return
			}

			if err != nil || res.Value != "5" || res.Variable != "x" {
				t.Fatalf("set x 5 = %+v, %v", res, err)
			}

			checkAfterSet(t, s)
		})
	}
}

// checkAfterSet: x changed to 5 shows in eval and in the changes; a set
// the adapter refuses fails but is logged.
func checkAfterSet(t *testing.T, s *Session) {
	t.Helper()

	if got := evalValue(t, s, "x"); got != "5" {
		t.Errorf("x = %s after set", got)
	}

	changed, err := s.Changes(t.Context(), 0)
	if err != nil || len(changed) != 1 || !slices.ContainsFunc(changed[0].Vars, func(v api.Var) bool { return v.Name == "x" && v.Value == "5" }) {
		t.Errorf("changes after set = %+v, %v; want x changed to 5", changed, err)
	}

	if _, err := s.Set(t.Context(), agentC, api.SetParams{Variable: "line", Value: "1"}); api.CodeOf(err) != api.CodeAdapterFailed {
		t.Errorf("set line = %v, want the adapter's refusal", err)
	}

	last := eventsOf(s, api.EventExec)
	if e := last[len(last)-1]; e.Action != ExecSet || e.Text != "line" {
		t.Errorf("exec event = %+v, want set line", e)
	}
}
