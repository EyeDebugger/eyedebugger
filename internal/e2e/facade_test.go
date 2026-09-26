// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/facade"
)

const humanE2E = "human:e2e"

// TestFacade drives a session from an editor's side through the real
// 'eyedbg dap' while the agent uses the CLI, once per language.
func TestFacade(t *testing.T) {
	t.Parallel()

	for _, lc := range langCases(t) {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()

			h := newHarness(t)
			if lc.lang == "fakelang" {
				fakeManifestFiles(t, h.configDir)
			}

			startAt(t, h, lc, "handoff")

			p := h.dap(humanE2E)
			thread := joinStopped(t, p, lc, lc.target)
			facadeInspect(t, p, lc, thread)
			facadeLease(t, h, p, lc, thread)
			facadeAgentView(t, h, lc)
			facadeLeave(t, h, p)
		})
	}
}

// startAt starts lc's program for the agent, stopped at the anchor.
func startAt(t *testing.T, h *harness, lc langCase, policy string) {
	t.Helper()

	args := append([]string{"start", lc.lang}, lc.startArgs(t)...)
	args = append(args, "--bp", loc(lc.file, lc.anchor), "--lease-policy", policy, "--timeout", startTimeout, "--json")

	var snap snapshotEnvelope

	h.run(args...).wantCode(exitOK).decode(&snap)

	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != lc.anchor {
		t.Fatalf("start: state=%s frame=%+v, want stopped at line %d", snap.Session.State, snap.Frame, lc.anchor)
	}
}

// joinStopped completes the handshake with a breakpoint at line (when not
// 0) and returns the thread of the replayed stop at the anchor.
func joinStopped(t *testing.T, p *dapProc, lc langCase, line int) int {
	t.Helper()

	caps, ok := p.ok(initializeReq()).(*godap.InitializeResponse)
	if !ok || !caps.Body.SupportsConfigurationDoneRequest || !caps.Body.SupportsLogPoints || caps.Body.SupportTerminateDebuggee {
		t.Fatalf("initialize = %+v", caps)
	}

	p.ok(&godap.AttachRequest{Request: request("attach"), Arguments: []byte(`{}`)})
	p.waitEvent("initialized")

	if line != 0 {
		bps, ok := p.ok(setBreakpointsReq(lc.file, line)).(*godap.SetBreakpointsResponse)
		if !ok || len(bps.Body.Breakpoints) != 1 || bps.Body.Breakpoints[0].Id == 0 || !bps.Body.Breakpoints[0].Verified {
			t.Fatalf("setBreakpoints = %+v", bps)
		}
	}

	p.ok(&godap.SetExceptionBreakpointsRequest{Request: request("setExceptionBreakpoints"), Arguments: godap.SetExceptionBreakpointsArguments{Filters: []string{}}})
	p.ok(&godap.ConfigurationDoneRequest{Request: request("configurationDone")})

	stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
	if !ok || stop.Body.ThreadId == 0 || stop.Body.PreserveFocusHint {
		t.Fatalf("replayed stop = %+v", stop)
	}

	if got := topLine(t, p, stop.Body.ThreadId); got != lc.anchor {
		t.Fatalf("joined at line %d, want the anchor %d", got, lc.anchor)
	}

	return stop.Body.ThreadId
}

// topFrame returns the top frame of thread.
func topFrame(t *testing.T, p *dapProc, thread int) godap.StackFrame {
	t.Helper()

	st, ok := p.ok(threadReq("stackTrace", thread)).(*godap.StackTraceResponse)
	if !ok || len(st.Body.StackFrames) == 0 {
		t.Fatalf("stackTrace = %+v", st)
	}

	return st.Body.StackFrames[0]
}

func topLine(t *testing.T, p *dapProc, thread int) int {
	t.Helper()

	return topFrame(t, p, thread).Line
}

// facadeInspect reads threads, scopes and variables at the anchor.
func facadeInspect(t *testing.T, p *dapProc, _ langCase, thread int) {
	t.Helper()

	threads, ok := p.ok(&godap.ThreadsRequest{Request: request("threads")}).(*godap.ThreadsResponse)
	if !ok || !slices.ContainsFunc(threads.Body.Threads, func(th godap.Thread) bool { return th.Id == thread }) {
		t.Fatalf("threads = %+v, want thread %d", threads, thread)
	}

	scopes, ok := p.ok(&godap.ScopesRequest{Request: request("scopes"), Arguments: godap.ScopesArguments{FrameId: topFrame(t, p, thread).Id}}).(*godap.ScopesResponse)
	if !ok || len(scopes.Body.Scopes) == 0 {
		t.Fatalf("scopes = %+v", scopes)
	}

	vars, ok := p.ok(&godap.VariablesRequest{Request: request("variables"), Arguments: godap.VariablesArguments{
		VariablesReference: scopes.Body.Scopes[0].VariablesReference,
	}}).(*godap.VariablesResponse)
	if !ok || len(vars.Body.Variables) == 0 {
		t.Fatalf("variables = %+v", vars)
	}
}

// facadeLease: the human can't continue under handoff until the agent
// grants the lease; then it runs to its own breakpoint, evaluates and
// steps.
func facadeLease(t *testing.T, h *harness, p *dapProc, lc langCase, thread int) {
	t.Helper()

	p.fails(threadReq("continue", thread), string(api.CodeLeaseHeld))
	h.run("lease", "grant", humanE2E).wantCode(exitOK)

	p.ok(threadReq("continue", thread))
	p.waitEvent("stopped")

	frame := topFrame(t, p, thread)
	if frame.Line != lc.target {
		t.Fatalf("continue stopped at line %d, want the human's breakpoint at %d", frame.Line, lc.target)
	}

	ev, ok := p.ok(evaluateReq(lc.okExpr, "watch", frame.Id)).(*godap.EvaluateResponse)
	if !ok || ev.Body.Result != lc.okValue {
		t.Errorf("evaluate %s = %+v, want %s", lc.okExpr, ev, lc.okValue)
	}

	p.fails(evaluateReq(lc.sideEffectExpr, "watch", frame.Id), string(api.CodeSideEffects))

	p.ok(threadReq("next", thread))
	p.waitEvent("stopped")
}

// facadeAgentView: the agent sees the human's breakpoint, requests and
// lease in the CLI, and can't continue while the human holds the lease.
func facadeAgentView(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	var bps struct {
		Breakpoints []api.Breakpoint `json:"breakpoints"`
	}

	h.run("--json", "bp", "ls").wantCode(exitOK).decode(&bps)

	if !slices.ContainsFunc(bps.Breakpoints, func(b api.Breakpoint) bool { return b.Owner == humanE2E && b.RequestedLine == lc.target }) {
		t.Errorf("bp ls = %+v, want %s's breakpoint at line %d", bps.Breakpoints, humanE2E, lc.target)
	}

	var events eventsEnvelope

	h.run("--json", "events", "--since", "0").wantCode(exitOK).decode(&events)

	seen := map[string]bool{}
	for i := range events.Events {
		e := &events.Events[i]
		seen[string(e.Kind)+":"+e.Action+":"+e.Client] = true
	}

	for _, want := range []string{"client::" + humanE2E, "exec:continue:" + humanE2E, "exec:next:" + humanE2E, "lease:grant:agent"} {
		if !seen[want] {
			t.Errorf("events lack %s", want)
		}
	}

	h.run("continue").wantCode(exitState).wantStderr("[" + string(api.CodeLeaseHeld) + "]")
}

// facadeLeave: disconnect ends 'eyedbg dap' with 0 and removes the
// human's breakpoints; the agent takes the lease back and stops.
func facadeLeave(t *testing.T, h *harness, p *dapProc) {
	t.Helper()

	p.ok(&godap.DisconnectRequest{Request: request("disconnect"), Arguments: &godap.DisconnectArguments{TerminateDebuggee: true}})

	if code := p.wait(); code != exitOK {
		t.Fatalf("eyedbg dap exited %d, want 0; stderr: %s", code, p.stderr)
	}

	var bps struct {
		Breakpoints []api.Breakpoint `json:"breakpoints"`
	}

	h.run("--json", "bp", "ls").wantCode(exitOK).decode(&bps)

	if slices.ContainsFunc(bps.Breakpoints, func(b api.Breakpoint) bool { return b.Owner == humanE2E }) {
		t.Errorf("bp ls after the human left = %+v", bps.Breakpoints)
	}

	h.run("lease", "take", "--force").wantCode(exitOK)
	h.run("stop").wantCode(exitOK)
}

// TestFacadeSessionEnds: the agent runs the program to its end while a
// human is joined; the human sees it resume, exit and terminate.
func TestFacadeSessionEnds(t *testing.T) {
	t.Parallel()

	for _, lc := range langCases(t) {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()

			h := newHarness(t)
			if lc.lang == "fakelang" {
				fakeManifestFiles(t, h.configDir)
			}

			startAt(t, h, lc, "free")

			// A session under an adapter this build doesn't trust the
			// exit code of (dotnet.ExitCodeUnknown) never sends a DAP
			// exited event (internal/facade/events.go): read the session's
			// real adapter (not the test process's, which may differ from
			// the daemon's) instead of assuming one.
			var st snapshotEnvelope
			h.run("--json", "status").wantCode(exitOK).decode(&st)
			untrusted := dotnet.ExitCodeUnknown(st.Session.Adapter, runtime.GOOS) != ""

			p := h.dap(humanE2E)
			joinStopped(t, p, lc, 0)

			h.run("bp", "rm", "all").wantCode(exitOK)
			h.run("continue", "--timeout", startTimeout).wantCode(exitOK)

			p.waitEvent("continued")

			if !untrusted {
				p.waitEvent("exited")
			}

			p.waitEvent("terminated")
			p.ok(&godap.DisconnectRequest{Request: request("disconnect")})

			if code := p.wait(); code != exitOK {
				t.Fatalf("eyedbg dap exited %d, want 0; stderr: %s", code, p.stderr)
			}

			h.run("stop").wantCode(exitOK)
		})
	}
}

// TestFacadeErrors: 'eyedbg dap' without a daemon or for an unknown
// session exits 2 with the error on stderr and nothing on stdout.
func TestFacadeErrors(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	r := h.runEnv(map[string]string{"EYEDBG_NO_AUTOSTART": "1"}, "dap")
	r.wantCode(exitState).wantStderr("[" + string(api.CodeNoSession) + "]")

	if len(r.stdout) != 0 {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}

	h.run("daemon", "start").wantCode(exitOK)

	r = h.run("dap", "-s", "s-none", "--json")
	r.wantCode(exitState).wantStderr(string(api.CodeNoSession))

	if len(r.stdout) != 0 {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

// TestFacadeCollab: an agent (CLI) and a human (DAP) share a session —
// the human sees the agent's breakpoints and actions, its copies of them
// never become the human's, a lease request reaches the agent, and the
// human's leaving releases the lease (docs/adr/0014).
func TestFacadeCollab(t *testing.T) {
	t.Parallel()

	for _, lc := range langCases(t) {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()

			h := newHarness(t)
			if lc.lang == "fakelang" {
				fakeManifestFiles(t, h.configDir)
			}

			startAt(t, h, lc, "handoff")

			p := h.dap(humanE2E)
			thread := joinStopped(t, p, lc, 0)
			anchor := collabJoin(t, h, p, lc)
			target := collabAgentBreakpoint(t, h, p, lc)
			collabEcho(t, h, p, anchor, target)
			collabAgentSteps(t, h, p)
			collabLeaseRequest(t, h, p, thread)
			collabLeave(t, h, p, anchor, target)
		})
	}
}

// agentBreakpoints returns the session's breakpoints as 'bp ls --json'
// lists them.
func agentBreakpoints(t *testing.T, h *harness) []api.Breakpoint {
	t.Helper()

	var bps struct {
		Breakpoints []api.Breakpoint `json:"breakpoints"`
	}

	h.run("--json", "bp", "ls").wantCode(exitOK).decode(&bps)

	return bps.Breakpoints
}

// isBreakpointEvent matches a breakpoint event with reason for id.
func isBreakpointEvent(reason string, id int) func(godap.EventMessage) bool {
	return func(ev godap.EventMessage) bool {
		b, ok := ev.(*godap.BreakpointEvent)

		return ok && b.Body.Reason == reason && b.Body.Breakpoint.Id == id
	}
}

// collabJoin: the join announced the agent's anchor breakpoint as a mirror
// and sent the lease, the clients and the breakpoints; the agent sees the
// human connected. It returns the mirror.
func collabJoin(t *testing.T, h *harness, p *dapProc, lc langCase) godap.Breakpoint {
	t.Helper()

	bps := agentBreakpoints(t, h)
	i := slices.IndexFunc(bps, func(b api.Breakpoint) bool { return b.Owner == "agent" && b.RequestedLine == lc.anchor })

	if i < 0 {
		t.Fatalf("bp ls = %+v, want the anchor breakpoint", bps)
	}

	ev, ok := p.waitEventWhere("breakpoint", isBreakpointEvent("new", bps[i].ID)).(*godap.BreakpointEvent)
	if !ok || ev.Body.Breakpoint.Column != 1 || ev.Body.Breakpoint.Source == nil || !strings.Contains(ev.Body.Breakpoint.Message, "agent's breakpoint") {
		t.Fatalf("mirror of the anchor = %+v", ev)
	}

	lease, ok := p.waitEvent(facade.CommandLease).(*facade.LeaseEvent)
	if !ok || lease.Body.Lease.Holder != "agent" {
		t.Errorf("eyedbg/lease at the join = %+v", lease)
	}

	clients, ok := p.waitEvent(facade.CommandClients).(*facade.ClientsEvent)
	if !ok || !slices.ContainsFunc(clients.Body.Clients, func(c api.ClientInfo) bool { return c.ID == humanE2E && c.Connected == 1 }) {
		t.Errorf("eyedbg/clients at the join = %+v", clients)
	}

	p.waitEvent(facade.CommandBreakpoints)

	var sessions struct {
		Sessions []api.SessionInfo `json:"sessions"`
	}

	h.run("--json", "sessions").wantCode(exitOK).decode(&sessions)

	if len(sessions.Sessions) != 1 || !slices.ContainsFunc(sessions.Sessions[0].Clients, func(c api.ClientInfo) bool {
		return c.ID == humanE2E && c.Connected == 1
	}) {
		t.Errorf("sessions = %+v, want %s connected", sessions.Sessions, humanE2E)
	}

	return ev.Body.Breakpoint
}

// collabAgentBreakpoint: the agent's new breakpoint (with a hit count,
// which the editor's copy loses) reaches the human as a mirror, an
// activity and a breakpoint list. It returns the mirror.
func collabAgentBreakpoint(t *testing.T, h *harness, p *dapProc, lc langCase) godap.Breakpoint {
	t.Helper()

	h.run("bp", "add", loc(lc.file, lc.target), "--hit", ">=1").wantCode(exitOK)

	i := slices.IndexFunc(agentBreakpoints(t, h), func(b api.Breakpoint) bool { return b.RequestedLine == lc.target })
	if i < 0 {
		t.Fatal("the agent's breakpoint at the target is not listed")
	}

	id := agentBreakpoints(t, h)[i].ID

	ev, ok := p.waitEventWhere("breakpoint", isBreakpointEvent("new", id)).(*godap.BreakpointEvent)
	if !ok || ev.Body.Breakpoint.Column != 1 || !strings.Contains(ev.Body.Breakpoint.Message, "hit >=1") {
		t.Fatalf("mirror of the target = %+v", ev)
	}

	p.waitEventWhere(facade.EventActivity, func(ev godap.EventMessage) bool {
		a, ok := ev.(*facade.ActivityEvent)

		return ok && a.Body.Event.Kind == api.EventBreakpoint && a.Body.Event.Action == "added" && a.Body.Event.Client == "agent"
	})
	p.waitEventWhere(facade.CommandBreakpoints, func(ev godap.EventMessage) bool {
		b, ok := ev.(*facade.BreakpointsEvent)

		return ok && slices.ContainsFunc(b.Body.Breakpoints, func(bp api.Breakpoint) bool { return bp.ID == id })
	})

	return ev.Body.Breakpoint
}

// collabEcho: the human's editor re-sends its copies, marked at column 1,
// as VS Code does: they stay the agent's, hit count included.
func collabEcho(t *testing.T, h *harness, p *dapProc, anchor, target godap.Breakpoint) {
	t.Helper()

	req := &godap.SetBreakpointsRequest{Request: request("setBreakpoints"), Arguments: godap.SetBreakpointsArguments{
		Source:      godap.Source{Path: target.Source.Path},
		Breakpoints: []godap.SourceBreakpoint{{Line: anchor.Line, Column: 1}, {Line: target.Line, Column: 1}},
	}}

	resp, ok := p.ok(req).(*godap.SetBreakpointsResponse)
	if !ok || len(resp.Body.Breakpoints) != 2 || resp.Body.Breakpoints[0].Id != anchor.Id || resp.Body.Breakpoints[1].Id != target.Id {
		t.Fatalf("setBreakpoints with the copies = %+v, want the agent's %d and %d", resp, anchor.Id, target.Id)
	}

	bps := agentBreakpoints(t, h)
	for i := range bps {
		if b := &bps[i]; b.Owner == humanE2E {
			t.Errorf("the human got a breakpoint from a copy: %+v", b)
		} else if b.ID == target.Id && (b.Owner != "agent" || b.HitCondition != ">=1") {
			t.Errorf("the agent's breakpoint after the echo = %+v", b)
		}
	}
}

// collabAgentSteps: the agent's step reaches the human as an activity, a
// resume and a stop that keeps the editor's focus.
func collabAgentSteps(t *testing.T, h *harness, p *dapProc) {
	t.Helper()

	h.run("next", "--timeout", startTimeout).wantCode(exitOK)

	// The exec event: continued, then its activity.
	p.waitEvent("continued")
	p.waitEventWhere(facade.EventActivity, func(ev godap.EventMessage) bool {
		a, ok := ev.(*facade.ActivityEvent)

		return ok && a.Body.Event.Kind == api.EventExec && a.Body.Event.Action == "next" && a.Body.Event.Client == "agent"
	})

	if stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent); !ok || !stop.Body.PreserveFocusHint {
		t.Errorf("stop after the agent's step = %+v, want preserveFocusHint", stop)
	}
}

// collabLeaseRequest: the human asks for the lease; the agent sees the
// request in its events and status, and grants it.
func collabLeaseRequest(t *testing.T, h *harness, p *dapProc, thread int) {
	t.Helper()

	var latest eventsEnvelope

	h.run("--json", "events", "--limit", "1").wantCode(exitOK).decode(&latest)

	req := &facade.LeaseRequest{Request: request(facade.CommandLease), Arguments: facade.LeaseArguments{Action: facade.LeaseActionRequest, Message: "e2e"}}
	if resp, ok := p.ok(req).(*facade.LeaseResponse); !ok || len(resp.Body.Lease.Requests) != 1 {
		t.Fatalf("eyedbg/lease request = %+v", resp)
	}

	var events eventsEnvelope

	h.run("--json", "events", "--since", strconv.Itoa(latest.Latest), "--kind", "lease", "--wait", "--timeout", "30s").wantCode(exitOK).decode(&events)

	if len(events.Events) == 0 || events.Events[0].Action != "request" || events.Events[0].Client != humanE2E || events.Events[0].Text != "e2e" {
		t.Errorf("lease events = %+v, want the human's request", events.Events)
	}

	h.run("status").wantCode(exitOK).wantStdout("requested by " + humanE2E)
	h.run("lease", "grant", humanE2E).wantCode(exitOK)

	p.waitEventWhere(facade.CommandLease, func(ev godap.EventMessage) bool {
		l, ok := ev.(*facade.LeaseEvent)

		return ok && l.Body.Lease.Holder == humanE2E && len(l.Body.Lease.Requests) == 0
	})

	p.ok(threadReq("next", thread))

	if stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent); !ok || stop.Body.PreserveFocusHint {
		t.Errorf("stop after the human's own step = %+v, want no preserveFocusHint", stop)
	}
}

// collabLeave: the disconnect retracts the copies before its response;
// the human's lease is released, the agent's breakpoints stay, and the
// agent drives again without --force.
func collabLeave(t *testing.T, h *harness, p *dapProc, anchor, target godap.Breakpoint) {
	t.Helper()

	p.ok(&godap.DisconnectRequest{Request: request("disconnect")})

	// The client hands events to onEvent before the response that follows
	// them reaches Do.
	got := p.received()
	for _, id := range []int{anchor.Id, target.Id} {
		if !slices.ContainsFunc(got, isBreakpointEvent("removed", id)) {
			t.Errorf("no removed event for %d before the disconnect response", id)
		}
	}

	if code := p.wait(); code != exitOK {
		t.Fatalf("eyedbg dap exited %d, want 0; stderr: %s", code, p.stderr)
	}

	var lease struct {
		Lease api.LeaseInfo `json:"lease"`
	}

	h.run("--json", "lease", "status").wantCode(exitOK).decode(&lease)

	if lease.Lease.Holder != "" {
		t.Errorf("lease after the human left = %+v, want nobody", lease.Lease)
	}

	var events eventsEnvelope

	h.run("--json", "events", "--kind", "client,lease", "--limit", "2").wantCode(exitOK).decode(&events)

	if n := len(events.Events); n != 2 || events.Events[0].Action != "disconnected" || events.Events[1].Action != "release" ||
		events.Events[1].Reason != "disconnected" {
		t.Errorf("last client and lease events = %+v, want disconnected then release (disconnected)", events.Events)
	}

	bps := agentBreakpoints(t, h)
	for i := range bps {
		if b := &bps[i]; b.Owner == humanE2E || (b.ID == target.Id && b.HitCondition != ">=1") {
			t.Errorf("breakpoint after the human left = %+v", b)
		}
	}

	h.run("continue", "--timeout", startTimeout).wantCode(exitOK)
	h.run("stop").wantCode(exitOK)
}
