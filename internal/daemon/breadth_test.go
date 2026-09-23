// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// TestBreadthMethods drives M5's methods over the socket.
func TestBreadthMethods(t *testing.T) {
	t.Parallel()

	ts := startServer(t, time.Hour)
	p := ts.paths
	snap := startFake(t, p, "", api.LeaseFree, "laps=3")
	ref := api.SessionRef{SessionID: snap.Session.ID}
	file := snap.Session.Program

	var fb api.Breakpoint
	mustCall(t, p, api.MethodBreakpointAdd, api.BreakpointAddParams{SessionRef: ref, BreakpointSpec: api.BreakpointSpec{Function: "f4"}}, &fb)

	if !fb.Verified || fb.Function != "f4" {
		t.Fatalf("function breakpoint = %+v", fb)
	}

	var lp api.Breakpoint
	mustCall(t, p, api.MethodBreakpointAdd, api.BreakpointAddParams{
		SessionRef: ref, BreakpointSpec: api.BreakpointSpec{File: file, Line: 2, LogMessage: "lap {lap}", HitCondition: ">=2"},
	}, &lp)

	var exc api.ExceptionsResult
	mustCall(t, p, api.MethodBreakpointExceptions, api.ExceptionsParams{SessionRef: ref, Mode: api.ExceptionsAll}, &exc)

	if len(exc.Modes) != 1 || exc.Modes[0].Client != api.DefaultClientID || len(exc.Filters) != 1 {
		t.Fatalf("exceptions = %+v", exc)
	}

	var ev api.EvalResult
	mustCall(t, p, api.MethodEval, api.EvalParams{SessionRef: ref, Expression: "$context", AllowSideEffects: true}, &ev)

	if ev.Value != "repl" {
		t.Errorf("eval with side effects ran in %q, want repl", ev.Value)
	}

	var set api.SetResult
	mustCall(t, p, api.MethodSet, api.SetParams{SessionRef: ref, Variable: "x", Value: "9"}, &set)

	if set.Value != "9" {
		t.Errorf("set = %+v", set)
	}

	checkBreadthRun(t, p, ref)
}

// checkBreadthRun continues through the laps: the function breakpoint
// stops in each, the logpoint logs from lap 2.
func checkBreadthRun(t *testing.T, p Paths, ref api.SessionRef) {
	t.Helper()

	for range 3 {
		var snap api.Snapshot
		mustCall(t, p, api.MethodExec, api.ExecParams{SessionRef: ref, Kind: "continue", Wait: api.Duration(20 * time.Second)}, &snap)

		if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != 4 {
			t.Fatalf("continue = %+v, want stopped at the function breakpoint", snap.Session)
		}
	}

	var out api.OutputResult
	mustCall(t, p, api.MethodOutput, api.OutputParams{SessionRef: ref}, &out)

	var logs []string

	for _, l := range out.Lines {
		if l.Category == "logpoint" {
			logs = append(logs, l.Text)
		}
	}

	if strings.Join(logs, "") != "lap 2\nlap 3\n" {
		t.Errorf("logpoint output = %q", logs)
	}
}

func TestAttachAndDetachMethods(t *testing.T) {
	t.Parallel()

	ts := startServer(t, time.Hour)
	p := ts.paths

	var snap api.Snapshot
	mustCall(t, p, api.MethodSessionStart, api.StartParams{Lang: "fake", Attach: &api.AttachSpec{PID: os.Getpid()}}, &snap)

	if snap.Session.Mode != api.ModeAttach || snap.Session.PID != os.Getpid() {
		t.Fatalf("attach = %+v", snap.Session)
	}

	var info api.SessionInfo
	mustCall(t, p, api.MethodSessionDetach, api.SessionRef{SessionID: snap.Session.ID}, &info)

	if info.State != api.StateExited || !strings.Contains(info.EndReason, "keeps running") {
		t.Fatalf("detach = %+v", info)
	}

	var list []api.SessionInfo
	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 0 {
		t.Errorf("sessions after detach = %+v, want none", list)
	}
}

func TestBreadthParamsAreChecked(t *testing.T) {
	t.Parallel()

	ts := startServer(t, time.Hour)
	p := ts.paths
	snap := startFake(t, p, humanID, api.LeaseFree)

	// Unknown fields are ignored (as for every method); bad values are not.
	tests := []struct {
		method string
		params any
	}{
		{api.MethodBreakpointExceptions, api.ExceptionsParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}, Mode: "sometimes"}},
		{api.MethodBreakpointExceptions, api.ExceptionsParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}, Force: true}},
		{api.MethodSet, api.SetParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}, Variable: "a[0", Value: "1"}},
		{api.MethodEval, json.RawMessage(`{"sessionId":"` + snap.Session.ID + `","expression":"x","depth":"deep"}`)},
	}

	for _, tt := range tests {
		if err := callAs(t, p, tt.method, tt.params, nil); api.CodeOf(err) != api.CodeInvalidRequest {
			t.Errorf("%s %v = %v, want INVALID_REQUEST", tt.method, tt.params, err)
		}
	}

	if err := callAs(t, p, api.MethodSessionDetach, api.SessionRef{SessionID: snap.Session.ID}, nil); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("detach of a launched session = %v, want INVALID_REQUEST", err)
	}
}
