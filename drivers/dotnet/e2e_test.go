// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// EnvE2E enables tests that need the .NET SDK and netcoredbg installed.
const envE2E = "EYEDBG_E2E"

// agent is the default client.
var agent = api.Client{ID: api.DefaultClientID, Kind: api.KindAgent}

const program = `var items = new List<int> { 7, 9 };
var total = 0;
for (var i = 1; i <= 3; i++)
{
    total += i;
    Console.WriteLine($"i={i} total={total}");
}
var name = "eyedbg";
Console.WriteLine($"done {name} {total}");
`

// newApp creates a console project with program as Program.cs.
func newApp(t *testing.T) string {
	t.Helper()

	host, err := dotnet.FindHost()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	cmd := exec.CommandContext(t.Context(), host, "new", "console", "-o", dir, "--name", "e2e")
	cmd.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dotnet new: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(dir, "Program.cs"), []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestDebugConsoleApp drives a real program through netcoredbg: breakpoints,
// locals, changed locals, expand, eval, stepping, conditional run-until,
// output and exit.
func TestDebugConsoleApp(t *testing.T) {
	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK and 'eyedbg adapters install netcoredbg')")
	}

	dir := newApp(t)
	src := filepath.Join(dir, "Program.cs")

	m := session.NewManager(t.Context(), session.Config{
		Drivers: []session.Driver{dotnet.New()}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard,
	})
	defer m.StopAll(t.Context())

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang:        "dotnet",
		LaunchSpec:  api.LaunchSpec{Project: dir},
		Breakpoints: []api.BreakpointSpec{{File: src, Line: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}

	changed := api.DumpSpec{Dump: []string{api.DumpChanged}}

	snap := sess.Wait(t.Context(), 0, time.Minute, changed)
	expectStop(t, snap, "breakpoint", 5)

	if snap.Changes == nil || !snap.Changes.NewFrame || changeOf(snap.Changes.Vars, "i") == nil {
		t.Errorf("first stop changes = %+v, want every local as new", snap.Changes)
	}

	inspectFirstStop(t, sess)
	stepOverAdd(t, sess)

	if _, _, err := sess.RemoveBreakpoint(t.Context(), agent, 0, false); err != nil {
		t.Fatal(err)
	}

	runUntilCondition(t, sess, src)

	snap, err = sess.Resume(t.Context(), agent, session.ExecContinue, 0, time.Minute, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 0 {
		t.Fatalf("after continue: %+v, want exited with code 0", snap.Session)
	}

	if out := joinOutput(sess.Output(0, 0).Lines); !strings.Contains(out, "done eyedbg 6") {
		t.Errorf("output = %q, want it to contain %q", out, "done eyedbg 6")
	}
}

// TestTwoClients shares one real session between an agent and a human:
// breakpoint ownership, one shared line, the handoff lease and the event
// log, through netcoredbg.
func TestTwoClients(t *testing.T) {
	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK and 'eyedbg adapters install netcoredbg')")
	}

	agentE2E := api.Client{ID: "agent:e2e", Kind: api.KindAgent, Name: "e2e"}
	humanE2E := api.Client{ID: "human:e2e", Kind: api.KindHuman, Name: "e2e"}

	dir := newApp(t)
	src := filepath.Join(dir, "Program.cs")

	m := session.NewManager(t.Context(), session.Config{
		Drivers: []session.Driver{dotnet.New()}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard,
	})
	defer m.StopAll(t.Context())

	sess, err := m.Start(t.Context(), agentE2E, api.StartParams{
		Lang: "dotnet", LaunchSpec: api.LaunchSpec{Project: dir}, LeasePolicy: api.LeaseHandoff,
		Breakpoints: []api.BreakpointSpec{{File: src, Line: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, time.Minute, api.DumpSpec{}), "breakpoint", 5)
	expectI(t, sess, "1")

	sess.Touch(humanE2E)

	h5 := addE2EBreakpoint(t, sess, humanE2E, src, 5)
	addE2EBreakpoint(t, sess, humanE2E, src, 8)

	for _, b := range sess.Breakpoints("") {
		if !b.Verified {
			t.Errorf("breakpoint %+v is not verified", b)
		}
	}

	if _, _, err := sess.RemoveBreakpoint(t.Context(), agentE2E, h5.ID, false); api.CodeOf(err) != api.CodeNotOwner {
		t.Errorf("agent removing the human's breakpoint: %v, want NOT_OWNER", err)
	}

	handOver(t, sess, agentE2E, humanE2E)

	expectStop(t, resumeAs(t, sess, humanE2E), "breakpoint", 5)
	expectI(t, sess, "2")

	if removed, kept, err := sess.RemoveBreakpoint(t.Context(), agentE2E, 0, false); err != nil || removed != 1 || kept != 2 {
		t.Errorf("agent rm all = removed %d, kept %d, %v; want 1 and 2", removed, kept, err)
	}

	// The line the agent shared with the human still stops.
	expectStop(t, resumeAs(t, sess, humanE2E), "breakpoint", 5)
	expectI(t, sess, "3")

	if _, _, err := sess.RemoveBreakpoint(t.Context(), humanE2E, h5.ID, false); err != nil {
		t.Fatal(err)
	}

	expectStop(t, resumeAs(t, sess, humanE2E), "breakpoint", 8)
	expectTwoClientEvents(t, sess)

	if _, err := m.Stop(t.Context(), agentE2E, sess.ID); api.CodeOf(err) != api.CodeLeaseHeld {
		t.Errorf("agent stop while the human holds the lease: %v, want LEASE_HELD", err)
	}

	if _, err := m.Stop(t.Context(), humanE2E, sess.ID); err != nil {
		t.Errorf("human stop: %v", err)
	}
}

// handOver checks that under handoff the human can neither run nor take
// the lease until the agent grants it.
func handOver(t *testing.T, sess *session.Session, from, to api.Client) {
	t.Helper()

	if _, err := sess.Resume(t.Context(), to, session.ExecContinue, 0, time.Minute, api.DumpSpec{}); api.CodeOf(err) != api.CodeLeaseHeld {
		t.Fatalf("continue without the lease: %v, want LEASE_HELD", err)
	}

	if _, err := sess.TakeLease(to, false); api.CodeOf(err) != api.CodeLeaseHeld {
		t.Fatalf("take under handoff: %v, want LEASE_HELD", err)
	}

	if _, err := sess.GrantLease(from, to.ID, false); err != nil {
		t.Fatal(err)
	}
}

func addE2EBreakpoint(t *testing.T, sess *session.Session, c api.Client, file string, line int) api.Breakpoint {
	t.Helper()

	b, err := sess.AddBreakpoint(t.Context(), c, api.BreakpointSpec{File: file, Line: line})
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func resumeAs(t *testing.T, sess *session.Session, c api.Client) api.Snapshot {
	t.Helper()

	snap, err := sess.Resume(t.Context(), c, session.ExecContinue, 0, time.Minute, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func expectI(t *testing.T, sess *session.Session, want string) {
	t.Helper()

	if res, err := sess.Eval(t.Context(), "i", 0); err != nil || res.Value != want {
		t.Errorf("i = %+v, %v; want %s", res, err, want)
	}
}

// expectTwoClientEvents checks the log has what the two clients did, in
// order.
func expectTwoClientEvents(t *testing.T, sess *session.Session) {
	t.Helper()

	res, err := sess.Events(t.Context(), api.EventsParams{Limit: 1000, Kinds: []api.EventKind{
		api.EventStarted, api.EventClient, api.EventLease, api.EventExec, api.EventBreakpoint,
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"started agent:e2e", "breakpoint:added agent:e2e", "client human:e2e", "breakpoint:added human:e2e",
		"breakpoint:added human:e2e", "lease:grant agent:e2e", "exec:continue human:e2e", "breakpoint:removed agent:e2e",
		"exec:continue human:e2e", "breakpoint:removed human:e2e", "exec:continue human:e2e",
	}

	next := 0

	for i := range res.Events {
		e := &res.Events[i]

		got := string(e.Kind)
		if e.Action != "" {
			got += ":" + e.Action
		}

		if next < len(want) && got+" "+e.Client == want[next] {
			next++
		}
	}

	if next != len(want) {
		t.Errorf("events lack %q (and what follows it) in order", want[next])
	}
}

// stepOverAdd steps over "total += i" (i = 1): only total changed, from 0.
func stepOverAdd(t *testing.T, sess *session.Session) {
	t.Helper()

	snap, err := sess.Resume(t.Context(), agent, session.ExecNext, 0, time.Minute, api.DumpSpec{Dump: []string{api.DumpChanged}})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, snap, "step", 6)

	if c := snap.Changes; c == nil || c.NewFrame || len(c.Vars) != 1 || c.Vars[0].Name != "total" || c.Vars[0].Previous != "0" {
		t.Errorf("changes after next = %+v, want only total, from 0", snap.Changes)
	}
}

// inspectFirstStop checks vars, expand and eval at the first stop (i = 1).
func inspectFirstStop(t *testing.T, sess *session.Session) {
	t.Helper()

	scopes, err := sess.Vars(t.Context(), 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := varValue(scopes, "i"); got != "1" {
		t.Errorf("i = %q, want 1", got)
	}

	scopes, err = sess.Expand(t.Context(), 0, "items[1]", 1, 0)
	if err != nil || len(scopes) != 1 || len(scopes[0].Vars) != 1 || scopes[0].Vars[0].Value != "9" {
		t.Errorf("expand items[1] = %+v, %v; want 9", scopes, err)
	}

	res, err := sess.Eval(t.Context(), "total + 41", 0)
	if err != nil || res.Value != "41" {
		t.Errorf("eval total + 41 = %+v, %v; want 41", res, err)
	}
}

// runUntilCondition runs to line 6 with i == 3: the output since the resume
// is in the snapshot and the temporary breakpoint is gone afterwards.
func runUntilCondition(t *testing.T, sess *session.Session, src string) {
	t.Helper()

	snap, err := sess.RunUntil(t.Context(), agent, api.BreakpointSpec{File: src, Line: 6, Condition: "i == 3"}, 0, time.Minute, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, snap, "breakpoint", 6)

	if snap.Reached == nil || !*snap.Reached {
		t.Errorf("run-until reached = %v, want true", snap.Reached)
	}

	if res, err := sess.Eval(t.Context(), "i", 0); err != nil || res.Value != "3" {
		t.Errorf("i after run-until i == 3: %+v, %v; want 3", res, err)
	}

	if !strings.Contains(joinOutput(snap.Output), "i=2 total=3") {
		t.Errorf("snapshot output = %q, want the lines printed since the resume", joinOutput(snap.Output))
	}

	if bps := sess.Breakpoints(""); len(bps) != 0 {
		t.Errorf("breakpoints after run-until = %+v, want the temporary one removed", bps)
	}
}

func joinOutput(lines []api.OutputLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
	}

	return b.String()
}

func changeOf(vars []api.Var, name string) *api.Var {
	for i := range vars {
		if vars[i].Name == name {
			return &vars[i]
		}
	}

	return nil
}

func expectStop(t *testing.T, snap api.Snapshot, reason string, line int) {
	t.Helper()

	s := snap.Session
	if s.State != api.StateStopped || s.Stop == nil || s.Stop.Reason != reason || snap.Frame == nil || snap.Frame.Line != line {
		t.Fatalf("snapshot = %+v (frame %+v), want stopped by %s at line %d", s, snap.Frame, reason, line)
	}
}

func varValue(scopes []api.Scope, name string) string {
	for _, sc := range scopes {
		for _, v := range sc.Vars {
			if v.Name == name {
				return v.Value
			}
		}
	}

	return ""
}
