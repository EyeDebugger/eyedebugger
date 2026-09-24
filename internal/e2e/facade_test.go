// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"slices"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
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
	if !ok || stop.Body.ThreadId == 0 {
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

			p := h.dap(humanE2E)
			joinStopped(t, p, lc, 0)

			h.run("bp", "rm", "all").wantCode(exitOK)
			h.run("continue", "--timeout", startTimeout).wantCode(exitOK)

			p.waitEvent("continued")
			p.waitEvent("exited")
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
