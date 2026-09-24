// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

const renderBase = "/work/app"

// renderTime is the time in every rendering fixture.
var renderTime = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func stoppedSnapshot() api.Snapshot {
	return api.Snapshot{
		Session: api.SessionInfo{
			ID: "s-k3f9", Lang: "dotnet", Program: "/work/app/bin/Debug/net10.0/app.dll", State: api.StateStopped,
			PID: 4242, CreatedAt: renderTime,
			Stop:    &api.StopInfo{Reason: "breakpoint", ThreadID: 4242},
			Lease:   &api.LeaseInfo{Policy: api.LeaseFree, Holder: "agent", Since: &renderTime},
			Clients: []api.ClientInfo{{Client: api.Client{ID: "agent", Kind: api.KindAgent}, FirstSeen: renderTime, LastSeen: renderTime}},
		},
		Frame: &api.Frame{Name: "Program.<Main>$()", File: "/work/app/Program.cs", Line: 4, Column: 5},
		Source: []api.SourceLine{
			{Line: 3, Text: "{"},
			{Line: 4, Text: "    total += i;", Current: true},
			{Line: 5, Text: "    Console.WriteLine(total);"},
		},
	}
}

// dumpSnapshot is a stop with every --dump part, output and a budget cut.
func dumpSnapshot() api.Snapshot {
	snap := stoppedSnapshot()
	snap.Output = []api.OutputLine{{Seq: 7, Category: "stdout", Text: "i=1 total=1\ni=2 "}, {Seq: 8, Category: "stdout", Text: "total=3\n"}}
	snap.OutputOmitted = 3
	snap.Changes = &api.Changes{Vars: []api.Var{
		{Name: "total", Type: "int", Value: "3", Change: "changed", Previous: "1"},
		{Name: "line", Type: "string", Value: `"x"`, Change: "new"},
	}}
	snap.Locals = &api.Scope{Name: "Locals", More: 2, Truncated: true, Vars: []api.Var{
		{Name: "i", Type: "int", Value: "2"},
		{Name: "total", Type: "int", Value: "3"},
	}}
	snap.Stack = []api.Frame{
		{Index: 0, Name: "Program.<Main>$()", File: "/work/app/Program.cs", Line: 4},
		{Index: 1, Name: "[External Code]"},
	}

	return snap
}

func TestSessionRendering(t *testing.T) {
	t.Parallel()

	code := 0
	exited := api.Snapshot{Session: api.SessionInfo{
		ID: "s-k3f9", Lang: "dotnet", State: api.StateExited, ExitCode: &code, EndReason: "the program terminated",
	}}
	running := api.Snapshot{Session: api.SessionInfo{ID: "s-k3f9", Lang: "dotnet", State: api.StateRunning}, TimedOut: true}
	scopes := []api.Scope{{Name: "Locals", More: 3, Vars: []api.Var{
		{Name: "total", Type: "int", Value: "0"},
		{Name: "order", Type: "Order", Value: "{Order}", HasChildren: true},
		{Name: "items", Type: "List<int>", Value: "Count = 2", HasChildren: true, Children: []api.Var{
			{Name: "[0]", Type: "int", Value: "7"}, {Name: "[1]", Type: "int", Value: "9"},
		}},
	}}}
	bps := []api.Breakpoint{
		{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 4, Line: 4, Verified: true},
		{ID: 2, Owner: "agent", File: "/other/Lib.cs", RequestedLine: 10, Line: 12, Verified: true},
		{ID: 3, Owner: "human:ijat", File: "/work/app/Late.cs", RequestedLine: 7, Line: 7, Message: "pending until the module loads"},
		{ID: 4, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 9, Line: 9, Verified: true, Condition: "i == 3", Temporary: true},
		{ID: 5, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 12, Line: 12, Verified: true, Condition: "n > 1", Note: "shares its line with another client's breakpoint that has a different condition: it stops there unconditionally"},
		{ID: 6, Owner: "human:ijat", File: "/work/app/Program.cs", RequestedLine: 12, Line: 12, Verified: true, Condition: "n > 2", Note: "shares its line with another client's breakpoint that has a different condition: it stops there unconditionally"},
		{ID: 7, Owner: "agent", Function: "Orders.Price", Verified: true},
		{ID: 8, Owner: "human:ijat", Function: "Orders.Missing", Message: "no function Orders.Missing"},
		{ID: 9, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 14, Line: 15, Anchor: "total += price", Verified: true, HitCondition: ">=3", Hits: 4},
		{
			ID: 10, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 20, Line: 20, Verified: true, LogMessage: "i={i} total={total}", Hits: 2,
			Note: "Program.cs changed after the session started: the program runs the code it was built from, so this line may not match it (restart the session to debug the new code)",
		},
	}

	tests := []struct {
		name   string
		golden string
		render func(*bytes.Buffer) error
	}{
		{"stopped", "snapshot_stopped.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, stoppedSnapshot(), false, renderBase) }},
		{"stopped json", "snapshot_stopped_json.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, stoppedSnapshot(), true, renderBase) }},
		{"exited", "snapshot_exited.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, exited, false, renderBase) }},
		{"timed out", "snapshot_timed_out.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, running, false, renderBase) }},
		{"sessions", "sessions.golden", func(b *bytes.Buffer) error {
			return writeSessions(b, []api.SessionInfo{stoppedSnapshot().Session, exited.Session}, false)
		}},
		{"sessions empty json", "sessions_empty_json.golden", func(b *bytes.Buffer) error { return writeSessions(b, nil, true) }},
		{"sessions lost", "sessions_lost.golden", func(b *bytes.Buffer) error {
			return writeSessions(b, []api.SessionInfo{stoppedSnapshot().Session, lostSession()}, false)
		}},
		{"sessions json clients", "sessions_json_clients.golden", func(b *bytes.Buffer) error {
			return writeSessions(b, []api.SessionInfo{sharedSnapshot().Session}, true)
		}},
		{"snapshot shared", "snapshot_shared.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, sharedSnapshot(), false, renderBase) }},
		{"events", "events.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, sampleEvents(), eventsView{}, false, renderBase)
		}},
		{"events json", "events_json.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, sampleEvents(), eventsView{}, true, renderBase)
		}},
		{"events newest", "events_newest.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, otherEvents(), eventsView{newest: true}, false, renderBase)
		}},
		{"events timeout", "events_timeout.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, api.EventsResult{Latest: 25, TimedOut: true}, eventsView{wait: true, timeout: 30 * time.Second}, false, renderBase)
		}},
		{"lease", "lease.golden", func(b *bytes.Buffer) error {
			if err := writeLease(b, api.LeaseResult{SessionID: "s-k3f9", Lease: api.LeaseInfo{Policy: api.LeaseFree, Holder: "agent", Since: &renderTime}}, false); err != nil {
				return err
			}

			return writeLease(b, api.LeaseResult{SessionID: "s-k3f9", Lease: api.LeaseInfo{Policy: api.LeaseHandoff}}, false)
		}},
		{"lease json", "lease_json.golden", func(b *bytes.Buffer) error {
			return writeLease(b, api.LeaseResult{SessionID: "s-k3f9", Lease: api.LeaseInfo{Policy: api.LeaseHandoff, Holder: "human:ijat", Since: &renderTime}}, true)
		}},
		{"stack", "stack.golden", func(b *bytes.Buffer) error {
			return writeStack(b, []api.Frame{
				{Index: 0, Name: "App.Orders.Total()", File: "/work/app/Orders.cs", Line: 18},
				{Index: 1, Name: "Program.<Main>$()", File: "/work/app/Program.cs", Line: 4},
				{Index: 2, Name: "[External Code]"},
			}, false, renderBase)
		}},
		{"vars", "vars.golden", func(b *bytes.Buffer) error { return writeVars(b, scopes, false) }},
		{"eval", "eval.golden", func(b *bytes.Buffer) error {
			return writeEval(b, api.EvalResult{Expression: "total * 2", Value: "12", Type: "int"}, false)
		}},
		{"breakpoints", "breakpoints.golden", func(b *bytes.Buffer) error { return writeBreakpoints(b, bps, false, renderBase) }},
		{"stopped dump", "snapshot_dump.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, dumpSnapshot(), false, renderBase) }},
		{"stopped dump json", "snapshot_dump_json.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, dumpSnapshot(), true, renderBase) }},
		{"new frame", "snapshot_new_frame.golden", func(b *bytes.Buffer) error {
			snap := stoppedSnapshot()
			snap.Changes = &api.Changes{NewFrame: true, Vars: []api.Var{{Name: "i", Type: "int", Value: "1", Change: "new"}}}

			return writeSnapshot(b, snap, false, renderBase)
		}},
		{"run-until missed", "snapshot_run_until_missed.golden", func(b *bytes.Buffer) error {
			snap, reached := stoppedSnapshot(), false
			snap.Reached, snap.Target = &reached, &api.BreakpointSpec{File: "/work/app/Program.cs", Line: 9}

			return writeSnapshot(b, snap, false, renderBase)
		}},
		{"vars none changed", "vars_none.golden", func(b *bytes.Buffer) error {
			return writeVars(b, []api.Scope{{Name: "Changed since the previous stop"}}, false)
		}},
		{"vars truncated", "vars_truncated.golden", func(b *bytes.Buffer) error {
			return writeVars(b, []api.Scope{{Name: "Locals", More: 4, Truncated: true, Vars: []api.Var{
				{Name: "order", Type: "Order", Value: "{Order}", HasChildren: true},
			}}}, false)
		}},
		{"exceptions", "exceptions.golden", func(b *bytes.Buffer) error {
			if err := writeExceptions(b, api.ExceptionsResult{SessionID: "s-k3f9", Modes: []api.ClientExceptionMode{}, Filters: []string{}}, false); err != nil {
				return err
			}

			return writeExceptions(b, sampleExceptions(), false)
		}},
		{"exceptions json", "exceptions_json.golden", func(b *bytes.Buffer) error { return writeExceptions(b, sampleExceptions(), true) }},
		{"eval depth", "eval_depth.golden", func(b *bytes.Buffer) error {
			return writeEval(b, api.EvalResult{
				Expression: "order", Value: "{Order}", Type: "Order", HasChildren: true, More: 3, Truncated: true,
				Children: []api.Var{{Name: "Id", Type: "int", Value: "42"}, {Name: "Items", Type: "List<Item>", Value: "Count = 2", HasChildren: true}},
			}, false)
		}},
		{"set", "set.golden", func(b *bytes.Buffer) error {
			return writeSet(b, api.SetResult{Variable: "total", Value: "100", Type: "int"}, false)
		}},
		{"set json", "set_json.golden", func(b *bytes.Buffer) error {
			return writeSet(b, api.SetResult{Variable: "total", Value: "100", Type: "int"}, true)
		}},
		{"snapshot exception", "snapshot_exception.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, exceptionSnapshot(), false, renderBase) }},
		{"snapshot exception json", "snapshot_exception_json.golden", func(b *bytes.Buffer) error {
			return writeSnapshot(b, exceptionSnapshot(), true, renderBase)
		}},
		{"detach", "detach.golden", func(b *bytes.Buffer) error {
			if err := writeEnded(b, api.SessionInfo{
				ID: "s-k3f9", Mode: api.ModeAttach, PID: 4242, State: api.StateExited, EndReason: "detached by agent (the program keeps running)",
			}); err != nil {
				return err
			}

			// An attached program that exited by itself doesn't keep running.
			if err := writeEnded(b, api.SessionInfo{ID: "s-9x2m", Mode: api.ModeAttach, PID: 4243, State: api.StateExited, EndReason: "the program terminated"}); err != nil {
				return err
			}

			return writeEnded(b, api.SessionInfo{ID: "s-7f3k", State: api.StateExited})
		}},
		{"events breadth", "events_breadth.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, breadthEvents(), eventsView{}, false, renderBase)
		}},
		{"lease request", "lease_request.golden", func(b *bytes.Buffer) error {
			held := api.LeaseResult{SessionID: "s-k3f9", HolderConnected: true, Lease: api.LeaseInfo{
				Policy: api.LeaseHandoff, Holder: "human:ijat", Since: &renderTime, Requests: []api.LeaseRequest{
					{Client: "agent:b", At: renderTime},
					{Client: "agent", Message: "I need to step into \"Total()\"\nnow", At: renderTime},
				},
			}}
			if err := writeLeaseRequest(b, held, "agent"); err != nil {
				return err
			}

			if err := writeLeaseRequest(b, api.LeaseResult{SessionID: "s-k3f9", Lease: api.LeaseInfo{Policy: api.LeaseHandoff}}, "agent"); err != nil {
				return err
			}

			return writeLeaseRequest(b, api.LeaseResult{SessionID: "s-k3f9", Lease: api.LeaseInfo{Policy: api.LeaseFree, Holder: "agent"}}, "agent")
		}},
		{"lease request json", "lease_request_json.golden", func(b *bytes.Buffer) error {
			return writeLease(b, api.LeaseResult{SessionID: "s-k3f9", HolderConnected: true, Lease: api.LeaseInfo{
				Policy: api.LeaseHandoff, Holder: "human:ijat", Since: &renderTime,
				Requests: []api.LeaseRequest{{Client: "agent", Message: "I need to step into Total()", At: renderTime}},
			}}, true)
		}},
		{"events presence", "events_presence.golden", func(b *bytes.Buffer) error {
			return writeEvents(b, presenceEvents(), eventsView{}, false, renderBase)
		}},
		{"breakpoints editor", "breakpoints_editor.golden", func(b *bytes.Buffer) error {
			return writeBreakpoints(b, []api.Breakpoint{
				{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 4, Line: 4, Verified: true},
				{ID: 2, Owner: "human:ijat", File: "/work/app/Program.cs", RequestedLine: 9, Line: 9, Verified: true, Editor: true},
				{ID: 3, Owner: "human:ijat", Function: "Orders.Price", Verified: true, Editor: true},
			}, false, renderBase)
		}},
		{"output", "output.golden", func(b *bytes.Buffer) error {
			return writeOutput(b, io.Discard, api.OutputResult{Lines: []api.OutputLine{{Seq: 1, Category: "stdout", Text: "i=1\n"}, {Seq: 2, Category: "stderr", Text: "warn"}}}, false, false)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := tt.render(&b); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

// lostSession is a session of a daemon that crashed.
func lostSession() api.SessionInfo {
	return api.SessionInfo{
		ID: "s-7f3k", Lang: "dotnet", Program: "/work/old/bin/Debug/net10.0/old.dll", State: api.StateLost,
		CreatedAt: renderTime.Add(-time.Hour), Recording: "/run/user/1000/eyedbg/sessions/s-7f3k.jsonl",
	}
}

// sharedSnapshot is a stop in a session two clients use, under handoff:
// the human holds the lease from a connected editor, and the agent asked
// for it.
func sharedSnapshot() api.Snapshot {
	snap := stoppedSnapshot()
	snap.Session.Lease = &api.LeaseInfo{
		Policy: api.LeaseHandoff, Holder: "human:ijat", Since: &renderTime,
		Requests: []api.LeaseRequest{{Client: "agent", Message: "I need to step into Total()", At: renderTime.Add(2 * time.Minute)}},
	}
	snap.Session.Clients = append(snap.Session.Clients, api.ClientInfo{
		Client: api.Client{ID: "human:ijat", Kind: api.KindHuman, Name: "ijat"}, FirstSeen: renderTime, LastSeen: renderTime.Add(time.Minute),
		Connected: 1,
	})
	snap.Session.Recording = "/run/user/1000/eyedbg/sessions/s-k3f9.jsonl"

	return snap
}

// sampleEvents is the example of the events help: two clients, a lease
// handed over and taken back, the program's end. Values are synthetic.
func sampleEvents() api.EventsResult {
	code := 0
	bp1 := api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 5, Line: 5, Verified: true, CreatedAt: renderTime}
	bp2 := api.Breakpoint{ID: 2, Owner: "human:ijat", File: "/work/app/Program.cs", RequestedLine: 9, Line: 9, Verified: true, Condition: "i == 3", CreatedAt: renderTime}
	events := []api.Event{
		{Kind: api.EventStarted, Client: "agent", Program: "/work/app/app.dll", Lease: &api.LeaseInfo{Policy: api.LeaseHandoff, Holder: "agent", Since: &renderTime}},
		{Kind: api.EventBreakpoint, Action: "added", Client: "agent", Breakpoint: &bp1},
		{Kind: api.EventThread, Reason: "started", ThreadID: 4242},
		{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "breakpoint", ThreadID: 4242}},
		{Kind: api.EventClient, Client: "human:ijat"},
		{Kind: api.EventBreakpoint, Action: "added", Client: "human:ijat", Breakpoint: &bp2},
		{Kind: api.EventLease, Action: "grant", Client: "agent", Previous: "agent", Lease: &api.LeaseInfo{Policy: api.LeaseHandoff, Holder: "human:ijat", Since: &renderTime}},
		{Kind: api.EventExec, Action: "continue", Client: "human:ijat"},
		{Kind: api.EventOutput, Category: "stdout", Text: "i=1 total=1\ni=2 total=3\n"},
		{Kind: api.EventBreakpoint, Action: "removed", Client: "agent", Breakpoint: &bp1},
		{Kind: api.EventLease, Action: "auto", Client: "agent", Previous: "human:ijat", Lease: &api.LeaseInfo{Policy: api.LeaseHandoff, Holder: "agent", Since: &renderTime}},
		{Kind: api.EventExec, Action: "continue", Client: "agent"},
		{Kind: api.EventExited, ExitCode: &code},
		{Kind: api.EventEnded, Reason: "the program terminated"},
	}

	for i := range events {
		events[i].Seq, events[i].Time = 12+i, renderTime
	}

	return api.EventsResult{Events: events, Latest: 25, Dropped: 11}
}

// otherEvents has every other form of event line, as the newest of more.
func otherEvents() api.EventsResult {
	pending := api.Breakpoint{ID: 3, Owner: "human:ijat", File: "/work/app/Late.cs", RequestedLine: 7, Line: 7, Message: "pending until the module loads"}
	moved := api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 5, Line: 6, Verified: true}
	temp := api.Breakpoint{ID: 4, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 20, Line: 20, Verified: true, Temporary: true}
	events := []api.Event{
		{Kind: api.EventLease, Action: "take", Client: "human:ijat"},
		{Kind: api.EventLease, Action: "force", Client: "agent", Previous: "human:ijat"},
		{Kind: api.EventLease, Action: "release", Client: "agent"},
		{Kind: api.EventLease, Action: "policy", Client: "agent", Lease: &api.LeaseInfo{Policy: api.LeaseHumanPriority}},
		{Kind: api.EventExec, Action: "stepIn", Client: "agent", ThreadID: 4243},
		{Kind: api.EventExec, Action: "runUntil", Client: "agent"},
		{Kind: api.EventBreakpoint, Action: "added", Client: "agent", Breakpoint: &temp},
		{Kind: api.EventContinued, ThreadID: 4242},
		{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "exception", ThreadID: 4242, Text: "System.InvalidOperationException: synthetic"}},
		{Kind: api.EventBreakpoint, Action: "removed", Client: "agent", Breakpoint: &pending},
		{Kind: api.EventBreakpoint, Action: "changed", Breakpoint: &moved},
		{Kind: api.EventBreakpoint, Action: "changed", Breakpoint: &pending},
		{Kind: api.EventOutput, Category: "stderr", Text: "warn", Truncated: true},
		{Kind: api.EventThread, Reason: "exited", ThreadID: 4243},
		{Kind: api.EventEnded, Reason: "stopped by human:ijat", Client: "human:ijat"},
	}

	for i := range events {
		events[i].Seq, events[i].Time = 40+i, renderTime
	}

	return api.EventsResult{Events: events, Latest: 54, More: 39}
}

// sampleExceptions has two clients' exception modes.
func sampleExceptions() api.ExceptionsResult {
	return api.ExceptionsResult{
		SessionID: "s-k3f9",
		Modes:     []api.ClientExceptionMode{{Client: "agent", Mode: api.ExceptionsAll}, {Client: "human:ijat", Mode: api.ExceptionsUncaught}},
		Filters:   []string{"all", "user-unhandled"},
	}
}

// exceptionSnapshot is a stop at a thrown exception; values are synthetic.
func exceptionSnapshot() api.Snapshot {
	snap := stoppedSnapshot()
	snap.Session.Stop = &api.StopInfo{Reason: "exception", ThreadID: 4242, Text: "Exception thrown: 'System.InvalidOperationException' in app.dll"}
	snap.Exception = &api.ExceptionInfo{
		ID: "CLR/System.InvalidOperationException", Description: "no orders", BreakMode: "always",
		Type: "System.InvalidOperationException", Message: "no orders",
		StackTrace: "   at Orders.Total() in /work/app/Orders.cs:line 18\n   at Orders.Sum() in /work/app/Orders.cs:line 12\n" +
			"   at Orders.Run() in /work/app/Orders.cs:line 9\n   at Program.Step(Int32 i) in /work/app/Program.cs:line 30\n" +
			"   at Program.Loop() in /work/app/Program.cs:line 22\n   at Program.<Main>$(String[] args) in /work/app/Program.cs:line 4\n" +
			"   at Program.Start() in /work/app/Program.cs:line 2",
		Inner: []api.ExceptionInfo{{Type: "System.ArgumentException", Message: "bad\nid"}},
	}

	return snap
}

// breadthEvents has the event lines of attach, test runs, exception
// modes, eval and set with side effects, and logpoints.
func breadthEvents() api.EventsResult {
	fbp := api.Breakpoint{ID: 3, Owner: "agent", Function: "Orders.Price", Verified: true, CreatedAt: renderTime}
	lp := api.Breakpoint{ID: 4, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 20, Line: 20, Verified: true, LogMessage: "i={i}", CreatedAt: renderTime}
	events := []api.Event{
		{Kind: api.EventStarted, Action: api.ModeAttach, Client: "agent", Program: "pid 4242 (dotnet)", Lease: &api.LeaseInfo{Policy: api.LeaseFree, Holder: "agent", Since: &renderTime}},
		{Kind: api.EventStarted, Action: api.ModeTest, Client: "agent", Program: "dotnet test tests.csproj --filter Adds", Lease: &api.LeaseInfo{Policy: api.LeaseFree, Holder: "agent", Since: &renderTime}},
		{Kind: api.EventExceptions, Action: "all", Client: "agent"},
		{Kind: api.EventExceptions, Action: "none", Client: "human:ijat", Reason: "force"},
		{Kind: api.EventBreakpoint, Action: "added", Client: "agent", Breakpoint: &fbp},
		{Kind: api.EventBreakpoint, Action: "added", Client: "agent", Breakpoint: &lp},
		{Kind: api.EventExec, Action: "eval", Client: "agent", Text: "Orders.Price(2)"},
		{Kind: api.EventExec, Action: "set", Client: "human:ijat", Text: "total"},
		{Kind: api.EventOutput, Category: "logpoint", Text: "i=3\n"},
		{Kind: api.EventEnded, Reason: "detached by agent (the program keeps running)", Client: "agent"},
	}

	for i := range events {
		events[i].Seq, events[i].Time = 1+i, renderTime
	}

	return api.EventsResult{Events: events, Latest: 10}
}

func TestParseLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want api.BreakpointSpec
	}{
		{"src/Program.cs:12", api.BreakpointSpec{File: absPath("src/Program.cs"), Line: 12}},
		{`C:\work\app\Program.cs:12`, api.BreakpointSpec{File: absPath(`C:\work\app\Program.cs`), Line: 12}},
		{"func:Orders.Price", api.BreakpointSpec{Function: "Orders.Price"}},
		{"func:12", api.BreakpointSpec{File: absPath("func"), Line: 12}},
		{`Program.cs@"total += price"`, api.BreakpointSpec{File: absPath("Program.cs"), Anchor: "total += price"}},
		{`Program.cs@"a:1 "quoted""`, api.BreakpointSpec{File: absPath("Program.cs"), Anchor: `a:1 "quoted"`}},
	}

	for _, tt := range tests {
		if got, err := parseLocation(tt.in); err != nil || got != tt.want {
			t.Errorf("parseLocation(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}

	for _, bad := range []string{"Program.cs", "Program.cs:0", "Program.cs:x", ":3", `Program.cs@"x`, `Program.cs@""`, `Program.cs@"  "`} {
		if _, err := parseLocation(bad); err == nil {
			t.Errorf("parseLocation(%q) succeeded, want an error", bad)
		}
	}
}

// presenceEvents is a human's editor connecting, asking for the lease,
// getting it and leaving.
func presenceEvents() api.EventsResult {
	handoff := func(holder string, requests ...api.LeaseRequest) *api.LeaseInfo {
		return &api.LeaseInfo{Policy: api.LeaseHandoff, Holder: holder, Since: &renderTime, Requests: requests}
	}
	events := []api.Event{
		{Kind: api.EventClient, Client: "human:ijat"},
		{Kind: api.EventClient, Action: "connected", Client: "human:ijat"},
		{
			Kind: api.EventLease, Action: "request", Client: "human:ijat", Text: "let me step through parse()",
			Lease: handoff("agent", api.LeaseRequest{Client: "human:ijat", Message: "let me step through parse()", At: renderTime}),
		},
		{Kind: api.EventLease, Action: "request", Client: "agent:b", Lease: handoff("agent")},
		{Kind: api.EventLease, Action: "grant", Client: "agent", Previous: "agent", Lease: handoff("human:ijat")},
		{Kind: api.EventClient, Action: "disconnected", Client: "human:ijat"},
		{Kind: api.EventLease, Action: "release", Client: "human:ijat", Previous: "human:ijat", Reason: "disconnected", Lease: handoff("")},
	}

	for i := range events {
		events[i].Seq, events[i].Time = 30+i, renderTime
	}

	return api.EventsResult{Events: events, Latest: 36}
}
