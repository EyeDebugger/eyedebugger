// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// e2eWait bounds every wait for the real program (building included).
const e2eWait = 3 * time.Minute

func e2eEval(t *testing.T, sess *session.Session, expr string) string {
	t.Helper()

	res, err := sess.Eval(t.Context(), agent, api.EvalParams{Expression: expr})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}

	return res.Value
}

func e2eResume(t *testing.T, sess *session.Session, kind string) api.Snapshot {
	t.Helper()

	snap, err := sess.Resume(t.Context(), agent, kind, 0, e2eWait, api.DumpSpec{})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}

	return snap
}

// TestBreadthLoop: an anchor breakpoint, set, eval with and without side
// effects, and a function breakpoint, through each adapter.
func TestBreadthLoop(t *testing.T) {
	forEachAdapter(t, breadthLoop)
}

func breadthLoop(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Program.cs")
	body := lineOf(t, src, "loop-body")

	sess, err := newManager(t).Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{"loop"}},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: "total += Orders.Price(i);"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", body)

	if bps := sess.Breakpoints(""); len(bps) != 1 || bps[0].Line != body || bps[0].Anchor == "" {
		t.Errorf("anchor breakpoint = %+v, want line %d", bps, body)
	}

	if res, err := sess.Set(t.Context(), agent, api.SetParams{Variable: "total", Value: "100"}); err != nil || res.Value != "100" {
		t.Fatalf("set total 100 = %+v, %v", res, err)
	}

	expectStop(t, e2eResume(t, sess, session.ExecNext), "step", body+1)

	if got := e2eEval(t, sess, "total"); got != "110" {
		t.Errorf("total after set 100 and one Price(1) = %s, want 110", got)
	}

	checkSideEffects(t, sess)
	checkFunctionBreakpoint(t, sess)
}

// checkSideEffects: a method call needs --allow-side-effects.
func checkSideEffects(t *testing.T, sess *session.Session) {
	t.Helper()

	if _, err := sess.Eval(t.Context(), agent, api.EvalParams{Expression: "Orders.Price(2)"}); api.CodeOf(err) != api.CodeSideEffects {
		t.Errorf("eval Orders.Price(2) = %v, want SIDE_EFFECTS", err)
	}

	res, err := sess.Eval(t.Context(), agent, api.EvalParams{Expression: "Orders.Price(2)", AllowSideEffects: true})
	if err != nil || res.Value != "20" {
		t.Errorf("eval Orders.Price(2) with side effects = %+v, %v; want 20", res, err)
	}
}

// checkFunctionBreakpoint: func:Orders.Price stops in Price (the line
// breakpoint at the call is removed first: it would stop before the call).
func checkFunctionBreakpoint(t *testing.T, sess *session.Session) {
	t.Helper()

	if _, _, err := sess.RemoveBreakpoint(t.Context(), agent, 0, false); err != nil {
		t.Fatal(err)
	}

	fb, err := sess.AddBreakpoint(t.Context(), agent, api.BreakpointSpec{Function: "Orders.Price"})
	if err != nil {
		t.Fatal(err)
	}

	snap := e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateStopped || snap.Frame == nil || !strings.Contains(snap.Frame.Name, "Price") {
		logEvents(t, sess)
		t.Fatalf("continue to func:Orders.Price = %+v (stop %+v, frame %+v), want stopped in Price", snap.Session, snap.Session.Stop, snap.Frame)
	}

	bps := sess.Breakpoints("")
	for i := range bps {
		if bps[i].ID == fb.ID && !bps[i].Verified {
			t.Errorf("function breakpoint %+v not verified after it stopped", bps[i])
		}
	}
}

// TestBreadthHitsAndLogpoints: --hit 3 stops once, at i == 3; a logpoint
// logs every lap and never stops (both emulated by the session).
func TestBreadthHitsAndLogpoints(t *testing.T) {
	forEachAdapter(t, breadthHitsAndLogpoints)
}

func breadthHitsAndLogpoints(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Program.cs")
	body := lineOf(t, src, "loop-body")

	sess, err := newManager(t).Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{"loop"}},
		Breakpoints: []api.BreakpointSpec{
			{File: src, Line: body, HitCondition: "3"},
			{File: src, Line: body + 1, LogMessage: "i={i}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", body)

	if got := e2eEval(t, sess, "i"); got != "3" {
		t.Errorf("--hit 3 stopped at i = %s, want 3", got)
	}

	snap := e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want exited (no more stops)", snap.Session)
	}

	expectExitCode(t, snap.Session, 0)

	var logs []string

	for _, l := range sess.Output(0, 0).Lines {
		if l.Category == "logpoint" {
			logs = append(logs, strings.TrimSpace(l.Text))
		}
	}

	if want := []string{"i=1", "i=2", "i=3", "i=4", "i=5"}; !slices.Equal(logs, want) {
		t.Errorf("logpoint output = %q, want %q", logs, want)
	}

	bps := sess.Breakpoints("")
	for i := range bps {
		if bps[i].Hits != 5 {
			t.Errorf("breakpoint %d hits = %d, want 5", bps[i].ID, bps[i].Hits)
		}
	}
}

// TestBreadthSharedConditions: breakpoints of the agent and a human at one
// line with different conditions each stop once, when their own condition
// holds (eyedbg evaluates both through the adapter), and the stop names
// whose breakpoint it is for; a third client's logpoint there logs every
// later pass and suppresses neither stop.
func TestBreadthSharedConditions(t *testing.T) {
	forEachAdapter(t, breadthSharedConditions)
}

func breadthSharedConditions(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Program.cs")
	body := lineOf(t, src, "loop-body")

	var (
		human  = api.Client{ID: "human:e2e", Kind: api.KindHuman, Name: "e2e"}
		logger = api.Client{ID: "agent:log", Kind: api.KindAgent}
	)

	sess, err := newManager(t).Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{"loop"}},
		Breakpoints: []api.BreakpointSpec{{File: src, Line: body, Condition: "i == 1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", body)
	expectLoopIndex(t, sess, 1)

	add := func(c api.Client, spec api.BreakpointSpec) api.Breakpoint {
		t.Helper()

		spec.File, spec.Line = src, body

		b, err := sess.AddBreakpoint(t.Context(), c, spec)
		if err != nil {
			t.Fatalf("%s: bp add %+v: %v", c.ID, spec, err)
		}

		return b
	}

	before := sess.Breakpoints(agent.ID)

	mine := add(agent, api.BreakpointSpec{Condition: "i == 2"})
	if len(before) != 1 || mine.ID != before[0].ID {
		t.Fatalf("re-added breakpoint %d, want the agent's one of %+v", mine.ID, before)
	}

	theirs := add(human, api.BreakpointSpec{Condition: "i == 4"})

	snap := e2eResume(t, sess, session.ExecContinue)
	expectStop(t, snap, "breakpoint", body)
	expectLoopIndex(t, sess, 2)
	expectStopFor(t, snap, api.StopBreakpoint{ID: mine.ID, Owner: agent.ID})

	add(logger, api.BreakpointSpec{LogMessage: "i={i}"})

	snap = e2eResume(t, sess, session.ExecContinue)
	expectStop(t, snap, "breakpoint", body)
	expectLoopIndex(t, sess, 4)
	expectStopFor(t, snap, api.StopBreakpoint{ID: theirs.ID, Owner: human.ID})

	snap = e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want exited (no more stops)", snap.Session)
	}

	expectExitCode(t, snap.Session, 0)

	var logs []string

	for _, l := range sess.Output(0, 0).Lines {
		if l.Category == "logpoint" {
			logs = append(logs, strings.TrimSpace(l.Text))
		}
	}

	if want := []string{"i=3", "i=4", "i=5"}; !slices.Equal(logs, want) {
		t.Errorf("logpoint output = %q, want %q", logs, want)
	}
}

// expectLoopIndex checks the loop index i at the stop.
func expectLoopIndex(t *testing.T, sess *session.Session, want int) {
	t.Helper()

	if got := e2eEval(t, sess, "i"); got != strconv.Itoa(want) {
		t.Fatalf("stopped at i = %s, want %d", got, want)
	}
}

// expectStopFor checks the breakpoints a stop is for.
func expectStopFor(t *testing.T, snap api.Snapshot, want ...api.StopBreakpoint) {
	t.Helper()

	if got := snap.Session.Stop.Breakpoints; !slices.Equal(got, want) {
		t.Errorf("stop for %+v, want %+v", got, want)
	}
}

// TestBreadthExceptions: all stops at a caught throw, uncaught doesn't,
// and an unhandled exception stops even with none.
func TestBreadthExceptions(t *testing.T) {
	forEachAdapter(t, breadthExceptions)
}

func breadthExceptions(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	orders := filepath.Join(dir, "Orders.cs")
	fail := lineOf(t, orders, "fail")
	m := newManager(t)

	start := func(scenario string, mode api.ExceptionMode) *session.Session {
		t.Helper()

		sess, err := m.Start(t.Context(), agent, api.StartParams{
			Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{scenario}}, Exceptions: mode,
		})
		if err != nil {
			t.Fatal(err)
		}

		return sess
	}

	sess := start("throw", api.ExceptionsAll)
	snap := sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	expectStop(t, snap, "exception", fail)

	if e := snap.Exception; e == nil || e.Type != "System.InvalidOperationException" || e.Message != "no orders: caught" {
		t.Errorf("exception = %+v, want InvalidOperationException 'no orders: caught'", snap.Exception)
	}

	if snap = e2eResume(t, sess, session.ExecContinue); snap.Session.State != api.StateExited {
		t.Errorf("after the caught exception: %+v, want exited", snap.Session)
	}

	sess = start("throw", api.ExceptionsUncaught)
	snap = sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	if snap.Session.State != api.StateExited {
		t.Errorf("throw with uncaught: %+v, want exited without stopping", snap.Session)
	}

	expectExitCode(t, snap.Session, 0)

	sess = start("crash", api.ExceptionsNone)
	snap = sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	expectStop(t, snap, "exception", fail)
	t.Logf("unhandled exception stop: %s", strconv.Quote(snap.Session.Stop.Text))
}

// logEvents logs the session's events, to explain a failure.
func logEvents(t *testing.T, sess *session.Session) {
	t.Helper()

	res, err := sess.Events(t.Context(), api.EventsParams{Limit: api.MaxEventsLimit}, 0)
	if err != nil {
		t.Log(err)

		return
	}

	for i := range res.Events {
		e := &res.Events[i]
		t.Logf("event %d %s %s %s %+v %+v", e.Seq, e.Kind, e.Action, e.Client, e.Stop, e.Breakpoint)
	}
}
