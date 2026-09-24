// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

const stopOnEntry = true

func startParams(entry bool, policy api.LeasePolicy, args ...string) api.StartParams {
	return api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: entry, Args: args}, LeasePolicy: policy}
}

func isReason(reason string) func(godap.EventMessage) bool {
	return func(ev godap.EventMessage) bool {
		s, ok := ev.(*godap.StoppedEvent)

		return ok && s.Body.Reason == reason
	}
}

// checkNone fails if an event named name arrived before the response to a
// threads request sent now: everything the facade sends before it is in.
func (tc *testClient) checkNone(name string) {
	tc.t.Helper()

	_, n := tc.response(tc.send("threads", ""))
	for _, ev := range tc.events[tc.cursor:n] {
		if ev.GetEvent().Event == name {
			tc.t.Errorf("unexpected %s event; events: %s", name, tc.eventNames())
		}
	}

	tc.cursor = n
}

// checkBefore sends a request and checks that its response comes before
// the event named name it causes.
func (tc *testClient) checkBefore(command, args, name string) {
	tc.t.Helper()

	r, n := tc.response(tc.send(command, args))
	if !r.GetResponse().Success {
		tc.t.Fatalf("%s failed: %s", command, errorText(r))
	}

	tc.checkNoneBefore(n, name, command)
	tc.waitEvent(name, nil)
}

func TestHandshakeErrors(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := join(t, s, humanC)

	tc.fails("threads", "", api.CodeInvalidRequest)
	tc.fails("attach", "{}", api.CodeInvalidRequest)
	tc.fails("initialize", `{"adapterID":"x","linesStartAt1":false}`, api.CodeInvalidRequest)
	tc.fails("initialize", `{"adapterID":"x","pathFormat":"uri"}`, api.CodeInvalidRequest)

	caps, ok := tc.ok("initialize", `{"adapterID":"x"}`).(*godap.InitializeResponse)
	if !ok || !caps.Body.SupportsLogPoints || caps.Body.SupportTerminateDebuggee || !caps.Body.SupportsConditionalBreakpoints {
		t.Errorf("capabilities = %+v", caps)
	}

	tc.fails("initialize", `{"adapterID":"x"}`, api.CodeInvalidRequest)
	tc.fails("threads", "", api.CodeInvalidRequest)
	tc.fails("launch", "{}", api.CodeInvalidRequest)
	tc.fails("attach", `{"session":"s-other"}`, api.CodeInvalidRequest)
	tc.fails("configurationDone", "", api.CodeInvalidRequest)
	tc.ok("attach", `{"session":"`+s.ID+`","anything":1}`)
	tc.waitEvent("initialized", nil)
	tc.ok("threads", "")
	tc.fails("next", `{"threadId":1}`, api.CodeInvalidRequest)
	tc.fails("evaluate", `{"expression":"x","context":"repl"}`, api.CodeInvalidRequest)
	tc.ok("configurationDone", "")
	tc.fails("configurationDone", "", api.CodeInvalidRequest)
	tc.fails("attach", "{}", api.CodeInvalidRequest)
	tc.fails("stepBack", `{"threadId":1}`, api.CodeInvalidRequest)

	if e := tc.fails("eyedbg/nothing", "{}", api.CodeInvalidRequest); !strings.Contains(e.Format, "does not support eyedbg/nothing") {
		t.Errorf("unknown command error = %+v", e)
	}

	tc.fails("next", `{"threadId":"one"}`, api.CodeInvalidRequest)
}

func TestJoinReplaysState(t *testing.T) {
	t.Parallel()

	t.Run("stopped", func(t *testing.T) {
		t.Parallel()

		tc := joinReady(t, startSession(t, fakeDriver{}, startParams(stopOnEntry, "")), humanC, "")

		stop, ok := tc.waitEvent("stopped", nil).(*godap.StoppedEvent)
		if !ok || stop.Body.Reason != "entry" || !stop.Body.AllThreadsStopped || stop.Body.ThreadId != 1 {
			t.Errorf("replayed stop = %+v", stop)
		}
	})

	t.Run("running", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(false, "", "hang"))
		waitState(t, s, api.StateRunning)

		tc := joinReady(t, s, humanC, "")
		tc.checkNone("stopped")
		tc.checkBefore("pause", `{"threadId":1}`, "stopped")
	})

	t.Run("exited", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(false, "", "lines=2"))
		waitState(t, s, api.StateExited)

		tc := joinReady(t, s, humanC, "")
		if ex, ok := tc.waitEvent("exited", nil).(*godap.ExitedEvent); !ok || ex.Body.ExitCode != 0 {
			t.Errorf("exited = %+v", ex)
		}

		tc.waitEvent("terminated", nil)
	})
}

// waitState waits until s is in state.
func waitState(t *testing.T, s *session.Session, state api.SessionState) {
	t.Helper()

	since := 0

	for s.Info().State != state {
		res := s.Follow(t.Context(), since)
		if len(res.Events) == 0 {
			t.Fatalf("session is %s, want %s", s.Info().State, state)
		}

		since = res.Events[len(res.Events)-1].Seq
	}
}

// TestOrdering: a response comes before every event its request causes.
func TestOrdering(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)

	file := s.Program
	tc.checkNone("breakpoint")

	if r := tc.ok("setBreakpoints", `{"source":{"path":`+jsonString(file)+`},"breakpoints":[{"line":5}]}`); r == nil {
		t.Fatal("no setBreakpoints response")
	}

	tc.checkBefore("next", `{"threadId":1}`, "stopped")
	tc.checkBefore("continue", `{"threadId":1}`, "stopped")
	tc.checkBefore("terminate", "", "terminated")
}

func TestOtherClientsResume(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)

	if _, err := s.Resume(t.Context(), agentC, session.ExecNext, 0, testWait, api.DumpSpec{}); err != nil {
		t.Fatal(err)
	}

	if c, ok := tc.waitEvent("continued", nil).(*godap.ContinuedEvent); !ok || c.Body.ThreadId != 1 || !c.Body.AllThreadsContinued {
		t.Errorf("continued = %+v", c)
	}

	tc.waitEvent("stopped", isReason("step"))
}

func TestLeaseHeld(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, api.LeaseHandoff))
	tc := joinReady(t, s, humanC, "")

	e := tc.fails("continue", `{"threadId":1}`, api.CodeLeaseHeld)
	if e.Id != 7006 || e.Variables["holder"] != agentC.ID || !e.ShowUser || !strings.Contains(e.Format, "held by agent") {
		t.Errorf("error = %+v", e)
	}

	e = tc.fails("terminate", "", api.CodeLeaseHeld)
	if !e.ShowUser {
		t.Errorf("terminate error = %+v, want showUser", e)
	}

	if e := tc.fails("exceptionInfo", `{"threadId":1}`, api.CodeAdapterFailed); e.ShowUser || e.Id != 7010 {
		t.Errorf("read error = %+v, want id 7010 without showUser", e)
	}

	if info := s.Info(); info.State != api.StateStopped || info.Lease.Holder != agentC.ID {
		t.Errorf("session after refused requests = %+v", info)
	}
}

// setBPs sends setBreakpoints for file with lines ("N" or "N if COND") and
// returns the answered breakpoints.
func (tc *testClient) setBPs(file string, lines ...string) []godap.Breakpoint {
	tc.t.Helper()

	specs := make([]string, 0, len(lines))

	for _, l := range lines {
		n, cond, _ := strings.Cut(l, " if ")
		spec := `{"line":` + n
		if cond != "" {
			spec += `,"condition":"` + cond + `"`
		}

		specs = append(specs, spec+"}")
	}

	r, ok := tc.ok("setBreakpoints", `{"source":{"path":`+jsonString(file)+`},"breakpoints":[`+strings.Join(specs, ",")+`]}`).(*godap.SetBreakpointsResponse)
	if !ok || len(r.Body.Breakpoints) != len(lines) {
		tc.t.Fatalf("setBreakpoints = %+v", r)
	}

	return r.Body.Breakpoints
}

// evalOK evaluates expr (as the agent, in watch) in s.
func evalOK(t *testing.T, s *session.Session, expr string) string {
	t.Helper()

	res, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: expr})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}

	return res.Value
}

func TestSetBreakpoints(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	agents, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: s.Program, Line: 3})
	if err != nil {
		t.Fatal(err)
	}

	tc := joinReady(t, s, humanC, "")

	first := tc.setBPs(s.Program, "3 if false", "5", "11")
	if first[0].Id == agents.ID || first[0].Id == 0 || !first[1].Verified || first[2].Verified || first[2].Line != 11 {
		t.Errorf("first = %+v", first)
	}

	again := tc.setBPs(s.Program, "5 if lap == 1", "3 if false")
	if again[0].Id != first[1].Id || again[1].Id != first[0].Id {
		t.Errorf("ids after a re-send = %+v, want %d, %d", again, first[1].Id, first[0].Id)
	}

	if got := evalOK(t, s, "$bps"); got != "3,5 if lap == 1" {
		t.Errorf("adapter holds %q", got)
	}

	checkRefusedBreakpoints(t, s, tc)

	if bps := s.Breakpoints(agentC.ID); len(bps) != 1 || bps[0].ID != agents.ID {
		t.Errorf("agent's breakpoints = %+v", bps)
	}

	checkFunctionBreakpoints(t, s, tc)
}

// TestTooManyBreakpoints: a list of more than maxRequestBreakpoints entries
// is refused whole, and changes nothing; one of exactly that many isn't.
func TestTooManyBreakpoints(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.setBPs(s.Program, "4")

	entries := func(n int, entry string) string {
		return "[" + strings.TrimSuffix(strings.Repeat(entry+",", n), ",") + "]"
	}

	for _, tt := range []struct{ command, args string }{
		{"setBreakpoints", `{"source":{"path":` + jsonString(s.Program) + `},"breakpoints":` + entries(maxRequestBreakpoints+1, `{"line":5}`) + `}`},
		{"setBreakpoints", `{"source":{"path":` + jsonString(s.Program) + `},"lines":` + entries(maxRequestBreakpoints+1, `5`) + `}`},
		{"setFunctionBreakpoints", `{"breakpoints":` + entries(maxRequestBreakpoints+1, `{"name":"f"}`) + `}`},
	} {
		tc.fails(tt.command, tt.args, api.CodeInvalidRequest)
	}

	if bps := s.Breakpoints(humanC.ID); len(bps) != 1 || bps[0].Line != 4 {
		t.Errorf("human's breakpoints = %+v, want the one at 4", bps)
	}

	r := tc.ok("setFunctionBreakpoints", `{"breakpoints":`+entries(maxRequestBreakpoints, `{"name":"f"}`)+`}`)
	if got, ok := r.(*godap.SetFunctionBreakpointsResponse); !ok || len(got.Body.Breakpoints) != maxRequestBreakpoints {
		t.Errorf("setFunctionBreakpoints of %d = %s", maxRequestBreakpoints, errorText(r))
	}
}

func checkFunctionBreakpoints(t *testing.T, s *session.Session, tc *testClient) {
	t.Helper()

	fn, ok := tc.ok("setFunctionBreakpoints", `{"breakpoints":[{"name":"f7"},{"name":"f8","hitCondition":"2"}]}`).(*godap.SetFunctionBreakpointsResponse)
	if !ok || fn.Body.Breakpoints[0].Id == 0 || fn.Body.Breakpoints[1].Id != 0 || evalOK(t, s, "$fbps") != "f7" {
		t.Errorf("function breakpoints = %+v, adapter %q", fn, evalOK(t, s, "$fbps"))
	}
}

// checkRefusedBreakpoints: a second breakpoint on a line and one in a
// missing file come back unverified, without an id.
func checkRefusedBreakpoints(t *testing.T, s *session.Session, tc *testClient) {
	t.Helper()

	dup := tc.setBPs(s.Program, "4", "4 if x")
	if dup[1].Id != 0 || dup[1].Verified || dup[1].Message == "" {
		t.Errorf("second breakpoint on a line = %+v", dup[1])
	}

	missing := tc.setBPs(s.Program+".none", "4")
	if missing[0].Verified || missing[0].Id != 0 {
		t.Errorf("breakpoint in a missing file = %+v", missing[0])
	}
}

// TestBreakpointEvents: the adapter's changes reach the owner, and another
// client's connection as changes to its mirrors (docs/adr/0014); another
// client's forced removal reaches the owner.
func TestBreakpointEvents(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{opts: daptest.Options{LateVerify: true}}, startParams(stopOnEntry, ""))
	owner := joinReady(t, s, humanC, "")
	other := joinReady(t, s, humanB, "")
	owner.waitEvent("stopped", nil)

	placed := owner.setBPs(s.Program, "4", "6")
	if placed[0].Verified {
		t.Fatalf("breakpoint verified before the resume: %+v", placed[0])
	}

	// The other connection announces both unverified before the resume
	// verifies them.
	checkMirrored(t, other, mirrorEvent{"new", placed[0].Id, false}, mirrorEvent{"new", placed[1].Id, false})

	_, n := owner.response(owner.send("continue", `{"threadId":1}`))
	owner.cursor = n

	changed, ok := owner.waitEvent("breakpoint", nil).(*godap.BreakpointEvent)
	if !ok || changed.Body.Reason != "changed" || !changed.Body.Breakpoint.Verified || changed.Body.Breakpoint.Id != placed[0].Id {
		t.Errorf("breakpoint event = %+v", changed)
	}

	checkMirrored(t, other, mirrorEvent{"changed", placed[0].Id, true})

	if _, _, err := s.RemoveBreakpoint(t.Context(), agentC, placed[1].Id, true); err != nil {
		t.Fatal(err)
	}

	removed, ok := owner.waitEvent("breakpoint", func(ev godap.EventMessage) bool {
		b, isBP := ev.(*godap.BreakpointEvent)

		return isBP && b.Body.Reason == "removed"
	}).(*godap.BreakpointEvent)
	if !ok || removed.Body.Breakpoint.Id != placed[1].Id {
		t.Errorf("removed event = %+v", removed)
	}
}

// mirrorEvent is a breakpoint event a connection should get for a mirror.
type mirrorEvent struct {
	reason   string
	id       int
	verified bool
}

// checkMirrored: the other connection got want, in order, each at column 1
// (the adapter's message may change a mirror in between).
func checkMirrored(t *testing.T, other *testClient, want ...mirrorEvent) {
	t.Helper()

	for _, w := range want {
		ev, ok := other.waitEvent("breakpoint", func(ev godap.EventMessage) bool {
			b, isBP := ev.(*godap.BreakpointEvent)

			return isBP && b.Body.Reason == w.reason && b.Body.Breakpoint.Id == w.id && b.Body.Breakpoint.Verified == w.verified
		}).(*godap.BreakpointEvent)
		if !ok || ev.Body.Breakpoint.Column != 1 {
			t.Errorf("other's %s event for %d = %+v", w.reason, w.id, ev)
		}
	}
}

func TestExceptionBreakpoints(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")

	tc.ok("setExceptionBreakpoints", `{"filters":["bogus","all"]}`)

	res, err := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{})
	if err != nil || len(res.Modes) != 1 || res.Modes[0].Client != humanC.ID || res.Modes[0].Mode != api.ExceptionsAll {
		t.Errorf("modes = %+v, %v", res, err)
	}

	tc.ok("setExceptionBreakpoints", `{"filters":["bogus"]}`)

	if res, _ := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{}); len(res.Modes) != 0 {
		t.Errorf("modes after unknown filters = %+v", res)
	}
}

func evalResult(t *testing.T, r godap.ResponseMessage) string {
	t.Helper()

	e, ok := r.(*godap.EvaluateResponse)
	if !ok {
		t.Fatalf("evaluate response = %s", errorText(r))
	}

	return e.Body.Result
}

// TestEvaluate: read contexts are guarded reads that name a frame, repl is
// an execution request, and duplicate keys decide the same for the policy
// and the adapter.
func TestEvaluate(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{sideEffects: callsCheck}, startParams(stopOnEntry, api.LeaseHandoff))
	tc := joinReady(t, s, humanC, "")

	if got := evalResult(t, tc.ok("evaluate", `{"expression":"$context","context":"hover","frameId":1}`)); got != "hover" {
		t.Errorf("hover evaluates in %q", got)
	}

	tc.fails("evaluate", `{"expression":"f(1)","context":"watch","frameId":1}`, api.CodeSideEffects)
	tc.fails("evaluate", `{"expression":"$context","context":"watch"}`, api.CodeInvalidRequest)
	tc.fails("evaluate", `{"expression":"$context","context":"clipboard","frameId":0}`, api.CodeInvalidRequest)
	tc.fails("evaluate", `{"expression":"$context","context":"repl"}`, api.CodeLeaseHeld)
	tc.fails("evaluate", `{"expression":"$context","context":"watch","context":"repl"}`, api.CodeLeaseHeld)

	if got := evalResult(t, tc.ok("evaluate", `{"expression":"$context","context":"repl","context":"watch","frameId":1}`)); got != "watch" {
		t.Errorf("repl then watch evaluates in %q", got)
	}

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	if got := evalResult(t, tc.ok("evaluate", `{"expression":"$context","context":"watch","context":"repl"}`)); got != "repl" {
		t.Errorf("watch then repl evaluates in %q", got)
	}

	execs, _ := s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{api.EventExec}}, 0)
	if len(execs.Events) != 1 || execs.Events[0].Action != "eval" || execs.Events[0].Client != humanC.ID {
		t.Errorf("exec events = %+v, want the human's one eval", execs.Events)
	}
}

// TestCompletionsColumn: completions with column < 1 (omitted, 0 or
// negative) is refused before the adapter sees it, the same as evaluate
// without a frame (S1) — debugpy would otherwise eval the raw text.
func TestCompletionsColumn(t *testing.T) {
	t.Parallel()

	caps := daptest.DefaultCaps()
	caps.SupportsCompletionsRequest = true

	s := startSession(t, fakeDriver{opts: daptest.Options{Caps: &caps}}, startParams(stopOnEntry, api.LeaseHandoff))
	tc := joinReady(t, s, humanC, "")

	tc.fails("completions", `{"text":"x"}`, api.CodeInvalidRequest)
	tc.fails("completions", `{"text":"x","column":0}`, api.CodeInvalidRequest)
	tc.fails("completions", `{"text":"x","column":-1}`, api.CodeInvalidRequest)
}

// TestInvalidated: another client's set invalidates the variables of
// clients that declared supportsInvalidatedEvent.
func TestInvalidated(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	actor := joinReady(t, s, humanC, "")
	aware := joinReady(t, s, humanB, "")
	unaware := joinReady(t, s, api.Client{ID: "human:c", Kind: api.KindHuman, Name: "c"},
		`{"adapterID":"x","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}`)

	actor.ok("setVariable", `{"variablesReference":1000,"name":"x","value":"7"}`)
	aware.waitEvent("invalidated", nil)
	unaware.checkNone("invalidated")
	actor.checkNone("invalidated")

	if got := evalOK(t, s, "x"); got != "7" {
		t.Errorf("x = %q after setVariable", got)
	}
}

func TestLogpointOutput(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")

	tc.ok("setBreakpoints", `{"source":{"path":`+jsonString(s.Program)+`},"breakpoints":[{"line":3,"logMessage":"at {line}"}]}`)
	tc.ok("continue", `{"threadId":1}`)

	out, ok := tc.waitEvent("output", func(ev godap.EventMessage) bool {
		o, isOut := ev.(*godap.OutputEvent)

		return isOut && strings.HasPrefix(o.Body.Output, "at ")
	}).(*godap.OutputEvent)
	if !ok || out.Body.Category != "console" || out.Body.Output != "at 3\n" {
		t.Errorf("logpoint output = %+v", out)
	}
}

// TestLeaveCleansUp: leaving, by disconnect or by closing, removes the
// connection's breakpoints and exception mode and releases its lease
// (docs/adr/0014); the session stays.
func TestLeaveCleansUp(t *testing.T) {
	t.Parallel()

	for _, how := range []string{"disconnect", "close"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()

			s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
			if _, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: s.Program, Line: 2}); err != nil {
				t.Fatal(err)
			}

			tc := joinReady(t, s, humanC, "")
			tc.setBPs(s.Program, "4", "6")
			tc.ok("setExceptionBreakpoints", `{"filters":["all"]}`)
			tc.ok("next", `{"threadId":1}`)

			if how == "disconnect" {
				tc.ok("disconnect", `{"terminateDebuggee":true}`)
			} else {
				_ = tc.conn.Close()
			}

			tc.waitClosed()
			checkCleanedUp(t, s)
		})
	}
}

func checkCleanedUp(t *testing.T, s *session.Session) {
	t.Helper()

	if bps := s.Breakpoints(""); len(bps) != 1 || bps[0].Owner != agentC.ID {
		t.Errorf("breakpoints after leaving = %+v, want the agent's only", bps)
	}

	if res, _ := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{}); len(res.Modes) != 0 {
		t.Errorf("exception modes after leaving = %+v", res.Modes)
	}

	if info := s.Info(); info.State == api.StateExited || info.Lease.Holder != "" {
		t.Errorf("session after leaving = %+v, want live with nobody holding the lease", info)
	}

	leases, _ := s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{api.EventLease}, Newest: true, Limit: 1}, 0)
	if n := len(leases.Events); n != 1 || leases.Events[0].Action != "release" || leases.Events[0].Reason != "disconnected" {
		t.Errorf("last lease event = %+v, want release (disconnected)", leases.Events)
	}

	if got := evalOK(t, s, "$bps"); got != "2" {
		t.Errorf("adapter holds %q", got)
	}
}

func TestSessionEnds(t *testing.T) {
	t.Parallel()

	t.Run("terminate", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
		tc := joinReady(t, s, humanC, "")
		tc.checkBefore("terminate", "", "terminated")

		if info := s.Info(); info.State != api.StateExited || !strings.Contains(info.EndReason, humanC.ID) {
			t.Errorf("session = %+v", info)
		}
	})

	t.Run("ended by another client", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(false, "", "hang"))
		tc := joinReady(t, s, humanC, "")

		if err := s.Terminate(t.Context(), agentC); err != nil {
			t.Fatal(err)
		}

		tc.waitEvent("terminated", nil)
		tc.fails("threads", "", api.CodeSessionExited)
	})

	t.Run("run to its end by another client", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
		tc := joinReady(t, s, humanC, "")
		tc.waitEvent("stopped", nil)

		if _, err := s.Resume(t.Context(), agentC, session.ExecContinue, 0, testWait, api.DumpSpec{}); err != nil {
			t.Fatal(err)
		}

		tc.waitEvent("continued", nil)
		tc.waitEvent("exited", nil)
		tc.waitEvent("terminated", nil)
		tc.ok("disconnect", "")
		tc.waitClosed()
	})
}

// TestFramingViolations: a malformed frame closes the connection without a
// reply; the session is unaffected.
func TestFramingViolations(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"long header":   strings.Repeat("A", 257) + "\r\n\r\n{}",
		"huge claim":    "Content-Length: 2097152\r\n\r\n{}",
		"garbage":       "hello\r\n\r\n",
		"not json":      "Content-Length: 3\r\n\r\n{{{",
		"json-rpc line": `{"jsonrpc":"2.0","id":3,"method":"daemon.status"}` + "\n",
	}

	for name, bytes := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
			tc := join(t, s, humanC)

			tc.rawBytes(bytes)
			tc.waitClosed()

			if len(tc.events) != 0 || len(tc.responses) != 0 || s.Info().State != api.StateStopped {
				t.Errorf("after %s: events %s, responses %d, session %s", name, tc.eventNames(), len(tc.responses), s.Info().State)
			}
		})
	}
}

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")

	const n = 100

	seqs := make([]int, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { seqs[i] = tc.send("threads", "") })
	}

	wg.Wait()

	for _, seq := range seqs {
		if r, _ := tc.response(seq); !r.GetResponse().Success {
			t.Errorf("threads %d: %s", seq, errorText(r))
		}
	}

	if len(tc.responses) != 0 {
		t.Errorf("%d responses left over", len(tc.responses))
	}
}

// TestJoinReconcilesBreakpoints: a breakpoint the adapter verified between
// its setBreakpoints response and configurationDone is announced at the
// join.
func TestJoinReconcilesBreakpoints(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{opts: daptest.Options{LateVerify: true}}, startParams(stopOnEntry, ""))
	tc := join(t, s, humanC)

	tc.ok("initialize", `{"adapterID":"x"}`)
	tc.ok("attach", "{}")
	tc.waitEvent("initialized", nil)

	placed := tc.setBPs(s.Program, "4")
	if placed[0].Verified {
		t.Fatalf("breakpoint verified at once: %+v", placed[0])
	}

	if _, err := s.Resume(t.Context(), agentC, session.ExecContinue, 0, testWait, api.DumpSpec{}); err != nil {
		t.Fatal(err)
	}

	tc.ok("configurationDone", "")

	b, ok := tc.waitEvent("breakpoint", nil).(*godap.BreakpointEvent)
	if !ok || b.Body.Reason != "changed" || b.Body.Breakpoint.Id != placed[0].Id || !b.Body.Breakpoint.Verified {
		t.Errorf("breakpoint event at the join = %+v", b)
	}
}

// TestPipelinedChangesApplyInOrder: requests that change something, sent
// in one write, apply in the order they were sent; the last one wins.
func TestPipelinedChangesApplyInOrder(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	source := `{"source":{"path":` + jsonString(s.Program) + `},"breakpoints":`

	var seqs []int

	var batch strings.Builder

	for _, r := range [][2]string{
		{"setBreakpoints", source + `[{"line":4}]}`},
		{"setExceptionBreakpoints", `{"filters":["all"]}`},
		{"setBreakpoints", source + `[]}`},
		{"setExceptionBreakpoints", `{"filters":[]}`},
	} {
		seq, msg := tc.request(r[0], r[1])
		seqs = append(seqs, seq)
		batch.WriteString(msg)
	}

	tc.rawBytes(batch.String())

	for _, seq := range seqs {
		if r, _ := tc.response(seq); !r.GetResponse().Success {
			t.Fatalf("request %d: %s", seq, errorText(r))
		}
	}

	if bps := s.Breakpoints(humanC.ID); len(bps) != 0 {
		t.Errorf("breakpoints after [4] then [] = %+v, want none", bps)
	}

	if res, _ := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{}); len(res.Modes) != 0 {
		t.Errorf("exception modes after all then none = %+v", res.Modes)
	}
}

// TestSourcelessBreakpointsKeepFunctionBreakpoints: setBreakpoints for a
// source without a path doesn't make the connection forget its function
// breakpoints, so leaving still removes them.
func TestSourcelessBreakpointsKeepFunctionBreakpoints(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")

	tc.ok("setFunctionBreakpoints", `{"breakpoints":[{"name":"f7"}]}`)
	tc.ok("setBreakpoints", `{"source":{"sourceReference":5},"breakpoints":[]}`)
	tc.ok("disconnect", "")
	tc.waitClosed()

	if bps := s.Breakpoints(humanC.ID); len(bps) != 0 {
		t.Errorf("breakpoints after leaving = %+v, want none", bps)
	}
}

// TestVerifiedDuringSetBreakpoints: the adapter verifies one of the
// connection's breakpoints while a setBreakpoints of the connection is in
// flight. The owner gets the breakpoint event after the response (D33),
// also when that response is the first to report the breakpoint to the
// connection (it was placed for the same client outside it).
func TestVerifiedDuringSetBreakpoints(t *testing.T) {
	t.Parallel()

	for _, placedBy := range []string{"connection", "session"} {
		t.Run(placedBy, func(t *testing.T) {
			t.Parallel()

			s := startSession(t, fakeDriver{opts: daptest.Options{LateVerify: true, VerifyOnSetBreakpoints: true}}, startParams(stopOnEntry, ""))
			tc := joinReady(t, s, humanC, "")
			id := placeUnverified(t, s, tc, placedBy == "connection")

			r, n := tc.response(tc.send("setBreakpoints", `{"source":{"path":`+jsonString(s.Program)+`},"breakpoints":[{"line":4}]}`))
			if resp, ok := r.(*godap.SetBreakpointsResponse); !ok || len(resp.Body.Breakpoints) != 1 || resp.Body.Breakpoints[0].Id != id {
				t.Fatalf("setBreakpoints = %s", errorText(r))
			}

			tc.checkNoneBefore(n, "breakpoint", "setBreakpoints")

			b, ok := tc.waitEvent("breakpoint", nil).(*godap.BreakpointEvent)
			if !ok || b.Body.Reason != "changed" || b.Body.Breakpoint.Id != id || !b.Body.Breakpoint.Verified {
				t.Errorf("breakpoint event = %+v", b)
			}
		})
	}
}

// placeUnverified places an unverified breakpoint on line 4 of s's
// program for tc's client, through tc or else through the session, and
// returns its id.
func placeUnverified(t *testing.T, s *session.Session, tc *testClient, throughConnection bool) int {
	t.Helper()

	if throughConnection {
		return tc.setBPs(s.Program, "4")[0].Id
	}

	got, err := s.ReplaceBreakpoints(t.Context(), humanC, s.Program, []api.BreakpointSpec{{Line: 4}}, nil)
	if err != nil || len(got) != 1 || got[0].Verified {
		t.Fatalf("placed = %+v, %v", got, err)
	}

	return got[0].ID
}

// checkNoneBefore fails if an event named name arrived before the
// response to command, which came after n events.
func (tc *testClient) checkNoneBefore(n int, name, command string) {
	tc.t.Helper()

	for _, ev := range tc.events[tc.cursor:n] {
		if ev.GetEvent().Event == name {
			tc.t.Fatalf("%s event before the %s response; events: %s", name, command, tc.eventNames())
		}
	}

	tc.cursor = n
}

// setBPsRaw sends setBreakpoints for file with the entries as JSON and
// returns the answer and how many events came before it.
func (tc *testClient) setBPsRaw(file, entries string) (answer []godap.Breakpoint, before int) {
	tc.t.Helper()

	r, n := tc.response(tc.send("setBreakpoints", `{"source":{"path":`+jsonString(file)+`},"breakpoints":`+entries+`}`))

	resp, ok := r.(*godap.SetBreakpointsResponse)
	if !ok || !r.GetResponse().Success {
		tc.t.Fatalf("setBreakpoints %s = %s", entries, errorText(r))
	}

	return resp.Body.Breakpoints, n
}

// isBreakpoint matches a breakpoint event with reason for id.
func isBreakpoint(reason string, id int) func(godap.EventMessage) bool {
	return func(ev godap.EventMessage) bool {
		b, ok := ev.(*godap.BreakpointEvent)

		return ok && b.Body.Reason == reason && b.Body.Breakpoint.Id == id
	}
}

// waitBreakpointEvent waits for a breakpoint event with reason for id.
func (tc *testClient) waitBreakpointEvent(reason string, id int) godap.Breakpoint {
	tc.t.Helper()

	b, ok := tc.waitEvent("breakpoint", isBreakpoint(reason, id)).(*godap.BreakpointEvent)
	if !ok {
		tc.t.Fatal("not a breakpoint event")
	}

	return b.Body.Breakpoint
}

// flush waits until everything the facade sent before now arrived, and
// returns the events received since the cursor (moving it past them).
func (tc *testClient) flush() []godap.EventMessage {
	tc.t.Helper()

	_, n := tc.response(tc.send("threads", ""))
	out := slices.Clone(tc.events[tc.cursor:n])
	tc.cursor = n

	return out
}

// named keeps the events named name.
func named(events []godap.EventMessage, name string) []godap.EventMessage {
	return slices.DeleteFunc(slices.Clone(events), func(ev godap.EventMessage) bool { return ev.GetEvent().Event != name })
}

// agentBP adds the agent's breakpoint at line with cond through the
// session.
func agentBP(t *testing.T, s *session.Session, c api.Client, line int, cond string) api.Breakpoint {
	t.Helper()

	b, err := s.AddBreakpoint(t.Context(), c, api.BreakpointSpec{File: s.Program, Line: line, Condition: cond})
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// joinWithMirror starts a stopped session where the agent has a
// breakpoint at 6 if x > 1, and joins the human, who got its mirror.
func joinWithMirror(t *testing.T) (s *session.Session, tc *testClient, agents api.Breakpoint) {
	t.Helper()

	s = startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	agents = agentBP(t, s, agentC, 6, "x > 1")
	tc = joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)
	tc.waitBreakpointEvent("new", agents.ID)

	return s, tc, agents
}

// TestMirrors: another client's line breakpoint is announced after the
// join as a breakpoint at column 1 whose message says whose it is and what
// it does, and eyedbg/breakpoints lists it.
func TestMirrors(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	agents := agentBP(t, s, agentC, 6, "x > 1")
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)

	b := tc.waitBreakpointEvent("new", agents.ID)
	if b.Line != 6 || b.Column != 1 || b.Source == nil || b.Source.Path != s.Program || !b.Verified {
		t.Errorf("mirror = %+v", b)
	}

	for _, want := range []string{"agent's breakpoint", "if x > 1", "hides"} {
		if !strings.Contains(b.Message, want) {
			t.Errorf("mirror message %q lacks %q", b.Message, want)
		}
	}

	list, ok := tc.waitEvent(CommandBreakpoints, nil).(*BreakpointsEvent)
	if !ok || len(list.Body.Breakpoints) != 1 || list.Body.Breakpoints[0].ID != agents.ID || list.Body.Breakpoints[0].Condition != "x > 1" {
		t.Errorf("eyedbg/breakpoints = %+v", list)
	}
}

// TestEchoKeepsOwnersCondition: the editor re-sending its copy of the
// agent's conditional breakpoint — marked at column 1, or plain where the
// connection announced it — answers with the agent's breakpoint: the human
// gets no breakpoint there and the adapter keeps the condition.
func TestEchoKeepsOwnersCondition(t *testing.T) {
	t.Parallel()

	for name, copyEntry := range map[string]string{"marked": `{"line":6,"column":1}`, "ruleA": `{"line":6}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, tc, agents := joinWithMirror(t)

			got, _ := tc.setBPsRaw(s.Program, `[{"line":4},`+copyEntry+`]`)
			if got[1].Id != agents.ID || !strings.Contains(got[1].Message, "if x > 1") || got[0].Id == agents.ID || got[0].Id == 0 {
				t.Errorf("answer = %+v, want the human's at 4 and the agent's %d", got, agents.ID)
			}

			for _, b := range s.Breakpoints("") {
				if b.Line == 6 && (b.Owner != agentC.ID || b.Condition != "x > 1") {
					t.Errorf("breakpoint at 6: %+v, want only the agent's, conditional", b)
				}
			}

			if bps := evalOK(t, s, "$bps"); bps != "4,6 if x > 1" {
				t.Errorf("adapter holds %q", bps)
			}

			tc.checkNone("breakpoint")
		})
	}
}

// TestHideAndReEnable: dropping the copy hides the mirror (the agent's
// breakpoint stays, and changes to it aren't announced); re-sending it
// marked shows it again; once hidden, a plain entry there is the human's own.
func TestHideAndReEnable(t *testing.T) {
	t.Parallel()

	s, tc, agents := joinWithMirror(t)

	tc.setBPsRaw(s.Program, `[{"line":4}]`)

	if bps := s.Breakpoints(""); len(bps) != 2 || s.Breakpoints(humanC.ID)[0].Line != 4 {
		t.Errorf("after hiding: %+v", bps)
	}

	agentBP(t, s, agentC, 6, "x > 2")
	tc.checkNone("breakpoint")

	got, _ := tc.setBPsRaw(s.Program, `[{"line":4},{"line":6,"column":1}]`)
	if got[1].Id != agents.ID || !strings.Contains(got[1].Message, "if x > 2") {
		t.Errorf("re-enabled copy = %+v, want the agent's %d", got[1], agents.ID)
	}

	tc.setBPsRaw(s.Program, `[{"line":4}]`)

	got, _ = tc.setBPsRaw(s.Program, `[{"line":4},{"line":6}]`)
	if got[1].Id == agents.ID || got[1].Id == 0 {
		t.Errorf("plain entry at a hidden mirror's line = %+v, want the human's own", got[1])
	}

	// An unconditional breakpoint makes its line unconditional (no note).
	if bps := evalOK(t, s, "$bps"); bps != "4,6" {
		t.Errorf("adapter holds %q", bps)
	}
}

// TestStaleCopyIsRetracted: a marked entry where no breakpoint is gets a
// negative id, then a removed event after the response, and a console line.
func TestStaleCopyIsRetracted(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)

	got, n := tc.setBPsRaw(s.Program, `[{"line":8,"column":1}]`)
	if got[0].Id >= 0 || got[0].Verified || got[0].Line != 8 || !strings.Contains(got[0].Message, "leftover copy") {
		t.Errorf("answer = %+v", got[0])
	}

	tc.checkNoneBefore(n, "breakpoint", "setBreakpoints")
	tc.waitBreakpointEvent("removed", got[0].Id)

	tc.waitEvent("output", func(ev godap.EventMessage) bool {
		o, ok := ev.(*godap.OutputEvent)

		return ok && o.Body.Category == "console" && strings.Contains(o.Body.Output, "removed 1 leftover copies")
	})

	if bps := s.Breakpoints(""); len(bps) != 0 {
		t.Errorf("breakpoints = %+v, want none", bps)
	}
}

// TestEditingACopyMakesItYours: a copy given a condition is the human's own
// breakpoint on that line; the mirror leaves without an event.
func TestEditingACopyMakesItYours(t *testing.T) {
	t.Parallel()

	s, tc, agents := joinWithMirror(t)

	got, _ := tc.setBPsRaw(s.Program, `[{"line":6,"column":1,"condition":"y"}]`)
	if got[0].Id == agents.ID || got[0].Id == 0 {
		t.Fatalf("answer = %+v, want a new breakpoint of the human's", got[0])
	}

	tc.checkNone("breakpoint")

	bps := s.Breakpoints("")
	if len(bps) != 2 || bps[0].ID != agents.ID || bps[0].Condition != "x > 1" || bps[1].Owner != humanC.ID || bps[1].Condition != "y" {
		t.Fatalf("breakpoints = %+v", bps)
	}

	if bps[0].Note == "" || bps[1].Note == "" {
		t.Errorf("breakpoints sharing a line without notes: %+v", bps)
	}
}

// aliasOf returns file through a symlink to its directory: another path
// of the same file, as an editor that opened it in a symlinked workspace
// has.
func aliasOf(t *testing.T, file string) string {
	t.Helper()

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(file), link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("can't create a symlink (Windows needs Developer Mode or an elevated shell): %v", err)
		}

		t.Fatal(err)
	}

	return filepath.Join(link, filepath.Base(file))
}

// humanLines checks the human's breakpoints are exactly one per line in
// want, by id.
func humanLines(t *testing.T, s *session.Session, want map[int]int) {
	t.Helper()

	got := map[int]int{}

	bps := s.Breakpoints(humanC.ID)
	for i := range bps {
		got[bps[i].Line] = bps[i].ID
	}

	if !maps.Equal(got, want) {
		t.Errorf("human's breakpoints by line = %v, want %v", got, want)
	}
}

// TestAliasPath: an editor that has the file under two paths (through a
// symlink, in either order) keeps its own breakpoints under both: a list
// under one path neither removes the breakpoints listed under the other
// nor hides a copy the editor holds there; new mirrors are announced under
// the path the editor uses.
func TestAliasPath(t *testing.T) {
	t.Parallel()

	t.Run("copy after own", func(t *testing.T) {
		t.Parallel()

		s, tc, agents := joinWithMirror(t)
		alias := aliasOf(t, s.Program)

		own, _ := tc.setBPsRaw(alias, `[{"line":4}]`)

		got, _ := tc.setBPsRaw(s.Program, `[{"line":6,"column":1}]`)
		if got[0].Id != agents.ID {
			t.Errorf("copy = %+v, want the agent's %d", got[0], agents.ID)
		}

		humanLines(t, s, map[int]int{4: own[0].Id})

		if slices.ContainsFunc(tc.flush(), isBreakpoint("removed", own[0].Id)) {
			t.Errorf("the editor was told its breakpoint %d under %s is removed", own[0].Id, alias)
		}

		if bps := evalOK(t, s, "$bps"); bps != "4,6 if x > 1" {
			t.Errorf("adapter holds %q", bps)
		}
	})

	t.Run("own after copy", func(t *testing.T) {
		t.Parallel()

		s, tc, agents := joinWithMirror(t)
		alias := aliasOf(t, s.Program)

		tc.setBPsRaw(s.Program, `[{"line":6,"column":1}]`)
		own, _ := tc.setBPsRaw(alias, `[{"line":4}]`)
		humanLines(t, s, map[int]int{4: own[0].Id})

		// The copy under the other path isn't hidden: changes still reach it.
		agentBP(t, s, agentC, 6, "x > 2")

		if b := tc.waitBreakpointEvent("changed", agents.ID); !strings.Contains(b.Message, "if x > 2") || b.Source == nil || b.Source.Path != s.Program {
			t.Errorf("changed = %+v, want the agent's new condition under %s", b, s.Program)
		}

		later := agentBP(t, s, agentC, 8, "")
		if b := tc.waitBreakpointEvent("new", later.ID); b.Source == nil || b.Source.Path != alias {
			t.Errorf("new mirror = %+v, want it under the editor's path %s", b, alias)
		}
	})

	t.Run("own under both", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
		tc := joinReady(t, s, humanC, "")
		tc.waitEvent("stopped", nil)
		alias := aliasOf(t, s.Program)

		viaAlias, _ := tc.setBPsRaw(alias, `[{"line":4}]`)

		viaReal, _ := tc.setBPsRaw(s.Program, `[{"line":4},{"line":8}]`)
		if viaReal[0].Id != viaAlias[0].Id {
			t.Errorf("line 4 under both paths = %d and %d, want one breakpoint", viaAlias[0].Id, viaReal[0].Id)
		}

		tc.setBPsRaw(s.Program, `[]`)
		humanLines(t, s, map[int]int{4: viaAlias[0].Id})

		tc.setBPsRaw(alias, `[]`)
		humanLines(t, s, map[int]int{})

		if bps := named(tc.flush(), "breakpoint"); len(bps) != 0 {
			t.Errorf("breakpoint events = %d, want none (the editor removed them itself)", len(bps))
		}
	})
}

// TestMirrorLifecycle: mirrors follow the other clients' breakpoints — one
// per line, never on a line of the connection's own.
func TestMirrorLifecycle(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{opts: daptest.Options{LateVerify: true}}, startParams(stopOnEntry, ""))
	agentB := api.Client{ID: "agent:b", Kind: api.KindAgent, Name: "b"}
	first := agentBP(t, s, agentC, 6, "")
	tc := joinReady(t, s, humanC, "")

	if b := tc.waitBreakpointEvent("new", first.ID); b.Verified || b.Column != 1 {
		t.Errorf("new = %+v, want unverified at column 1", b)
	}

	second := agentBP(t, s, agentC, 8, "")
	tc.waitBreakpointEvent("new", second.ID)

	if _, err := s.Resume(t.Context(), agentC, session.ExecNext, 0, testWait, api.DumpSpec{}); err != nil {
		t.Fatal(err)
	}

	// Wait for the change that verified it (the adapter's message may change
	// it before).
	verified, ok := tc.waitEvent("breakpoint", func(ev godap.EventMessage) bool {
		b, isBP := ev.(*godap.BreakpointEvent)

		return isBP && isBreakpoint("changed", first.ID)(ev) && b.Body.Breakpoint.Verified
	}).(*godap.BreakpointEvent)
	if !ok || verified.Body.Breakpoint.Column != 1 || verified.Body.Breakpoint.Line != 6 {
		t.Errorf("changed = %+v, want verified at 6:1", verified)
	}

	sameLine := agentBP(t, s, agentB, 6, "")

	// The editor's list holds its copies, marked, and a line of its own.
	tc.setBPsRaw(s.Program, `[{"line":6,"column":1},{"line":8,"column":1},{"line":9}]`)

	ownLine := agentBP(t, s, agentB, 9, "")

	for _, ev := range named(tc.flush(), "breakpoint") {
		if b, ok := ev.(*godap.BreakpointEvent); ok && b.Body.Reason == "new" && (b.Body.Breakpoint.Id == sameLine.ID || b.Body.Breakpoint.Id == ownLine.ID) {
			t.Errorf("announced %+v: a second breakpoint on a line, or on the human's line", b.Body.Breakpoint)
		}
	}

	if _, _, err := s.RemoveBreakpoint(t.Context(), agentC, first.ID, false); err != nil {
		t.Fatal(err)
	}

	tc.waitBreakpointEvent("removed", first.ID)
	tc.waitBreakpointEvent("new", sameLine.ID)
}

// TestOwnCLIBreakpointsSurvive: the human's breakpoints set outside the
// editor are shown as theirs, and neither the editor's list nor leaving
// removes them.
func TestOwnCLIBreakpointsSurvive(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	cli := agentBP(t, s, humanC, 5, "")
	tc := joinReady(t, s, humanC, "")

	if b := tc.waitBreakpointEvent("new", cli.ID); !strings.HasPrefix(b.Message, "your breakpoint, set outside this editor") {
		t.Errorf("mirror of the human's CLI breakpoint = %+v", b)
	}

	tc.setBPs(s.Program, "4")
	tc.ok("disconnect", "")
	tc.waitClosed()

	if bps := s.Breakpoints(humanC.ID); len(bps) != 1 || bps[0].ID != cli.ID || bps[0].Editor {
		t.Errorf("human's breakpoints after leaving = %+v, want the CLI one", bps)
	}
}

// TestRetractOnTheWayOut: every mirror is removed before the disconnect
// response, or before terminated; nothing follows the disconnect response.
func TestRetractOnTheWayOut(t *testing.T) {
	t.Parallel()

	t.Run("disconnect", func(t *testing.T) {
		t.Parallel()

		s, tc, agents := joinWithMirror(t)
		other := agentBP(t, s, agentC, 8, "")
		tc.waitBreakpointEvent("new", other.ID)

		_, n := tc.response(tc.send("disconnect", ""))
		for _, id := range []int{agents.ID, other.ID} {
			if !slices.ContainsFunc(tc.events[tc.cursor:n], isBreakpoint("removed", id)) {
				t.Errorf("no removed event for %d before the disconnect response; events: %s", id, tc.eventNames())
			}
		}

		agentBP(t, s, agentC, 9, "")
		tc.waitClosed()

		if after := tc.events[n:]; len(after) != 0 {
			t.Errorf("events after the disconnect response: %d", len(after))
		}
	})

	t.Run("ended", func(t *testing.T) {
		t.Parallel()

		s, tc, agents := joinWithMirror(t)
		if err := s.Terminate(t.Context(), agentC); err != nil {
			t.Fatal(err)
		}

		tc.waitBreakpointEvent("removed", agents.ID)
		tc.waitEvent("terminated", nil)
	})
}

// clientsOf finds client id in an eyedbg/clients event.
func clientsOf(ev godap.EventMessage, id string) (api.ClientInfo, bool) {
	c, ok := ev.(*ClientsEvent)
	if !ok {
		return api.ClientInfo{}, false
	}

	i := slices.IndexFunc(c.Body.Clients, func(ci api.ClientInfo) bool { return ci.ID == id })
	if i < 0 {
		return api.ClientInfo{}, false
	}

	return c.Body.Clients[i], true
}

// connectedIs matches an eyedbg/clients event where client id has n
// connections.
func connectedIs(id string, n int) func(godap.EventMessage) bool {
	return func(ev godap.EventMessage) bool {
		c, ok := clientsOf(ev, id)

		return ok && c.Connected == n
	}
}

// TestPresence: connections count as presence; the client's last one
// leaving releases its lease and removes its editor breakpoints, not an
// earlier one.
func TestPresence(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, api.LeaseHandoff))
	observer := joinReady(t, s, humanB, "")
	first := joinReady(t, s, humanC, "")
	observer.waitEvent(CommandClients, connectedIs(humanC.ID, 1))

	second := joinReady(t, s, humanC, "")
	observer.waitEvent(CommandClients, connectedIs(humanC.ID, 2))

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	first.setBPs(s.Program, "4")
	first.ok("disconnect", "")
	first.waitClosed()
	observer.waitEvent(CommandClients, connectedIs(humanC.ID, 1))

	if l := s.Lease(); l.Holder != humanC.ID || len(s.Breakpoints(humanC.ID)) != 1 {
		t.Errorf("after the first leave: lease %+v, breakpoints %+v", l, s.Breakpoints(humanC.ID))
	}

	_ = second.conn.Close()
	second.waitClosed()
	observer.waitEvent(CommandClients, connectedIs(humanC.ID, 0))

	if l := s.Lease(); l.Holder != "" || len(s.Breakpoints(humanC.ID)) != 0 {
		t.Errorf("after the last leave: lease %+v, breakpoints %+v", l, s.Breakpoints(humanC.ID))
	}

	for _, c := range s.Info().Clients {
		if c.Connected != map[string]int{humanB.ID: 1}[c.ID] {
			t.Errorf("client %s connected %d", c.ID, c.Connected)
		}
	}

	events, _ := s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{api.EventClient, api.EventLease}, Limit: api.MaxEventsLimit}, 0)

	var got []string

	for _, e := range events.Events {
		if e.Client == humanC.ID {
			got = append(got, string(e.Kind)+":"+e.Action+":"+e.Reason)
		}
	}

	want := []string{"client::", "client:connected:", "client:connected:", "client:disconnected:", "client:disconnected:", "lease:release:disconnected"}
	if !slices.Equal(got, want) {
		t.Errorf("human's client and lease events = %v, want %v", got, want)
	}

	if _, err := s.Resume(t.Context(), agentC, session.ExecNext, 0, testWait, api.DumpSpec{}); err != nil {
		t.Errorf("agent's next after the human left: %v", err)
	}
}

// TestRestartKeepsLease: a disconnect asking for a restart keeps the lease
// and the editor's breakpoints for the grace, and the next connection's
// re-send keeps their ids; without a grace they go at once.
func TestRestartKeepsLease(t *testing.T) {
	t.Parallel()

	for name, grace := range map[string]time.Duration{"grace": time.Hour, "none": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := startSession(t, fakeDriver{}, startParams(stopOnEntry, api.LeaseHandoff))
			if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
				t.Fatal(err)
			}

			tc := joinWith(t, Config{Session: s, Client: humanC, RestartGrace: grace})
			tc.handshake("")
			placed := tc.setBPs(s.Program, "4")
			tc.ok("disconnect", `{"restart":true}`)
			tc.waitClosed()

			if grace == 0 {
				if l := s.Lease(); l.Holder != "" || len(s.Breakpoints(humanC.ID)) != 0 {
					t.Errorf("without a grace: lease %+v, breakpoints %+v", l, s.Breakpoints(humanC.ID))
				}

				return
			}

			again := joinWith(t, Config{Session: s, Client: humanC, RestartGrace: grace})
			again.handshake("")

			if got := again.setBPs(s.Program, "4"); got[0].Id != placed[0].Id {
				t.Errorf("re-sent after the restart = %+v, want id %d", got[0], placed[0].Id)
			}

			if l := s.Lease(); l.Holder != humanC.ID {
				t.Errorf("lease after the restart = %+v, want the human's", l)
			}
		})
	}
}

// leaseOf returns an eyedbg/lease response's lease.
func leaseOf(t *testing.T, r godap.ResponseMessage) api.LeaseInfo {
	t.Helper()

	l, ok := r.(*LeaseResponse)
	if !ok {
		t.Fatalf("eyedbg/lease = %s", errorText(r))
	}

	return l.Body.Lease
}

// TestLeaseCustomRequest: eyedbg/lease does what the lease commands do,
// and its event follows its response.
func TestLeaseCustomRequest(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, api.LeaseHandoff))

	early := join(t, s, humanC)
	early.fails(CommandLease, `{"action":"status"}`, api.CodeInvalidRequest)
	early.ok("initialize", `{"adapterID":"x"}`)
	early.fails(CommandLease, `{"action":"status"}`, api.CodeInvalidRequest)

	observer := joinReady(t, s, humanB, "")
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent(CommandLease, nil)

	if l := leaseOf(t, tc.ok(CommandLease, `{"action":"status"}`)); l.Holder != agentC.ID {
		t.Errorf("status = %+v", l)
	}

	if e := tc.fails(CommandLease, `{"action":"take"}`, api.CodeLeaseHeld); e.Variables["holder"] != agentC.ID || e.ShowUser {
		t.Errorf("take under handoff = %+v", e)
	}

	checkLeaseRequest(t, tc, observer)

	for _, bad := range []string{`{"action":"steal"}`, `{}`, `{"action":"policy"}`, `{"action":7}`} {
		tc.fails(CommandLease, bad, api.CodeInvalidRequest)
	}

	if l := leaseOf(t, tc.ok(CommandLease, `{"action":"take","force":true}`)); l.Holder != humanC.ID || len(l.Requests) != 0 {
		t.Errorf("take --force = %+v", l)
	}

	if l := leaseOf(t, tc.ok(CommandLease, `{"action":"grant","to":"human:b"}`)); l.Holder != humanB.ID {
		t.Errorf("grant = %+v", l)
	}

	if l := leaseOf(t, observer.ok(CommandLease, `{"action":"policy","policy":"free"}`)); l.Policy != api.LeaseFree {
		t.Errorf("policy = %+v", l)
	}

	if l := leaseOf(t, observer.ok(CommandLease, `{"action":"release"}`)); l.Holder != "" {
		t.Errorf("release = %+v", l)
	}
}

// hasRequestBy matches an eyedbg/lease event with one pending request, by
// client.
func hasRequestBy(client string) func(godap.EventMessage) bool {
	return func(ev godap.EventMessage) bool {
		l, ok := ev.(*LeaseEvent)

		return ok && len(l.Body.Lease.Requests) == 1 && l.Body.Lease.Requests[0].Client == client
	}
}

// checkLeaseRequest: tc's request is answered with it pending, then its
// eyedbg/lease event reaches tc (after the response) and observer.
func checkLeaseRequest(t *testing.T, tc, observer *testClient) {
	t.Helper()

	r, n := tc.response(tc.send(CommandLease, `{"action":"request","message":"let me step"}`))
	if l := leaseOf(t, r); len(l.Requests) != 1 || l.Requests[0].Client != humanC.ID || l.Requests[0].Message != "let me step" {
		t.Errorf("request = %+v", l)
	}

	tc.checkNoneBefore(n, CommandLease, CommandLease)
	tc.waitEvent(CommandLease, hasRequestBy(humanC.ID))
	observer.waitEvent(CommandLease, hasRequestBy(humanC.ID))
}

// TestBreakpointsCustomRequest: eyedbg/breakpoints lists every client's
// breakpoints and removes one as 'eyedbg bp rm' does.
func TestBreakpointsCustomRequest(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	agents := agentBP(t, s, agentC, 3, "")
	owner := joinReady(t, s, humanC, "")
	placed := owner.setBPs(s.Program, "5")
	remover := joinReady(t, s, humanB, "")
	remover.waitBreakpointEvent("new", placed[0].Id)

	list, ok := remover.ok(CommandBreakpoints, `{}`).(*BreakpointsResponse)
	if !ok || len(list.Body.Breakpoints) != 2 || list.Body.Breakpoints[0].ID != agents.ID || !list.Body.Breakpoints[1].Editor {
		t.Errorf("list = %+v", list)
	}

	remover.fails(CommandBreakpoints, `{"action":"remove","id":`+strconv.Itoa(placed[0].Id)+`}`, api.CodeNotOwner)
	remover.fails(CommandBreakpoints, `{"action":"remove","id":0}`, api.CodeInvalidRequest)
	remover.fails(CommandBreakpoints, `{"action":"drop"}`, api.CodeInvalidRequest)

	r, n := remover.response(remover.send(CommandBreakpoints, `{"action":"remove","id":`+strconv.Itoa(placed[0].Id)+`,"force":true}`))
	if resp, ok := r.(*BreakpointsResponse); !ok || resp.Body.Removed != 1 || len(resp.Body.Breakpoints) != 1 {
		t.Fatalf("remove --force = %s", errorText(r))
	}

	remover.checkNoneBefore(n, "breakpoint", CommandBreakpoints)
	remover.waitBreakpointEvent("removed", placed[0].Id)
	owner.waitBreakpointEvent("removed", placed[0].Id)
}

// activityOf returns the eyedbg/activity events' session events.
func activityOf(events []godap.EventMessage) []api.Event {
	var out []api.Event

	for _, ev := range named(events, EventActivity) {
		if a, ok := ev.(*ActivityEvent); ok {
			out = append(out, a.Body.Event)
		}
	}

	return out
}

// TestActivity: other clients' actions reach the editor as
// eyedbg/activity; its own, the adapter's and output never do.
func TestActivity(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{opts: daptest.Options{LateVerify: true}}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.setBPs(s.Program, "8")
	tc.ok("next", `{"threadId":1}`)
	tc.waitEvent("stopped", isReason("step"))

	if _, err := s.Resume(t.Context(), agentC, session.ExecNext, 0, testWait, api.DumpSpec{}); err != nil {
		t.Fatal(err)
	}

	agentBP(t, s, agentC, 6, "")

	if _, err := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{Mode: api.ExceptionsAll}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	tc.waitEvent(EventActivity, func(ev godap.EventMessage) bool {
		a, ok := ev.(*ActivityEvent)

		return ok && a.Body.Event.Kind == api.EventLease && a.Body.Event.Action == "grant"
	})

	var got []string

	for _, e := range activityOf(tc.events) {
		if e.Client != agentC.ID {
			t.Errorf("activity of %q: %+v", e.Client, e)
		}

		got = append(got, string(e.Kind)+":"+e.Action)
	}

	want := []string{"lease:auto", "exec:next", "breakpoint:added", "exceptions:all", "lease:grant"}
	if !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v", got, want)
	}
}

// TestOutputReplay: the output held at the join is replayed once, before
// the program's state; beyond 200 chunks a console line counts the rest.
func TestOutputReplay(t *testing.T) {
	t.Parallel()

	t.Run("stopped", func(t *testing.T) {
		t.Parallel()

		s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
		for range 2 {
			if _, err := s.Resume(t.Context(), agentC, session.ExecNext, 0, testWait, api.DumpSpec{}); err != nil {
				t.Fatal(err)
			}
		}

		tc := joinReady(t, s, humanC, "")
		tc.waitEvent("stopped", nil)

		before := tc.events[:tc.cursor]
		if got := outputs(before); !slices.Equal(got, []string{"line 1\n", "line 2\n"}) {
			t.Errorf("output before the stop = %q", got)
		}

		if got := outputs(tc.flush()); len(got) != 0 {
			t.Errorf("output after the stop = %q, want none (replayed once)", got)
		}
	})

	t.Run("more than 200 chunks", func(t *testing.T) {
		t.Parallel()

		checkReplayCap(t)
	})

	t.Run("exited", func(t *testing.T) {
		t.Parallel()

		checkReplayExited(t)
	})
}

// outputs returns the output events' texts.
func outputs(events []godap.EventMessage) []string {
	var out []string

	for _, ev := range named(events, "output") {
		if o, ok := ev.(*godap.OutputEvent); ok {
			out = append(out, o.Body.Output)
		}
	}

	return out
}

// checkReplayCap: beyond 200 chunks, a console line counts the ones left
// out, then the newest 200 come.
func checkReplayCap(t *testing.T) {
	t.Helper()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, "", "laps=25"))
	agentBP(t, s, agentC, 5, "lap == 25")

	if _, err := s.Resume(t.Context(), agentC, session.ExecContinue, 0, testWait, api.DumpSpec{}); err != nil {
		t.Fatal(err)
	}

	held, _ := s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{api.EventOutput}, Limit: api.MaxEventsLimit}, 0)
	total := len(held.Events)

	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("stopped", nil)

	got := outputs(tc.events[:tc.cursor])
	if len(got) != replayChunks+1 || !strings.Contains(got[0], strconv.Itoa(total-replayChunks)+" earlier output chunks") ||
		got[len(got)-1] != "line 4\n" {
		t.Errorf("replayed %d chunks (of %d), first %q, last %q", len(got), total, got[0], got[len(got)-1])
	}
}

// checkReplayExited: joining an exited session replays its output, then
// its end.
func checkReplayExited(t *testing.T) {
	t.Helper()

	s := startSession(t, fakeDriver{}, startParams(false, "", "lines=2"))
	waitState(t, s, api.StateExited)

	tc := joinReady(t, s, humanC, "")
	tc.waitEvent("terminated", nil)

	var names []string
	for _, ev := range tc.events {
		names = append(names, ev.GetEvent().Event)
	}

	if !slices.Equal(names, []string{"initialized", "output", "output", "exited", "terminated"}) {
		t.Errorf("events = %v", names)
	}
}

// TestBreakpointsEventCoalesced: breakpoint changes in one follower batch
// send one eyedbg/breakpoints event.
func TestBreakpointsEventCoalesced(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, ""))
	tc := joinReady(t, s, humanC, "")
	tc.waitEvent(CommandBreakpoints, nil)

	for _, line := range []int{3, 5, 7} {
		agentBP(t, s, agentC, line, "")
	}

	tc.waitEvent(CommandBreakpoints, func(ev godap.EventMessage) bool {
		b, ok := ev.(*BreakpointsEvent)

		return ok && len(b.Body.Breakpoints) == 3
	})

	if n := len(named(tc.events, CommandBreakpoints)); n < 2 || n > 4 {
		t.Errorf("%d eyedbg/breakpoints events (1 at the join), want 1 to 3 for the three changes", n)
	}
}
