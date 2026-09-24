// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"strings"
	"sync"
	"testing"

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

// TestBreakpointEvents: the adapter's changes reach the owner only; another
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

	_, n := owner.response(owner.send("continue", `{"threadId":1}`))
	owner.cursor = n

	changed, ok := owner.waitEvent("breakpoint", nil).(*godap.BreakpointEvent)
	if !ok || changed.Body.Reason != "changed" || !changed.Body.Breakpoint.Verified || changed.Body.Breakpoint.Id != placed[0].Id {
		t.Errorf("breakpoint event = %+v", changed)
	}

	other.checkNone("breakpoint")

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
// connection's breakpoints and exception mode; the session and the lease
// stay.
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

	if info := s.Info(); info.State == api.StateExited || info.Lease.Holder != humanC.ID {
		t.Errorf("session after leaving = %+v, want live with the human's lease", info)
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

	got, err := s.ReplaceBreakpoints(t.Context(), humanC, s.Program, []api.BreakpointSpec{{Line: 4}})
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
