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

	m := session.NewManager(t.Context(), []session.Driver{dotnet.New()}, slog.New(slog.DiscardHandler), io.Discard, nil)
	defer m.StopAll(t.Context())

	sess, err := m.Start(t.Context(), api.StartParams{
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

	if _, err := sess.RemoveBreakpoint(t.Context(), 0); err != nil {
		t.Fatal(err)
	}

	runUntilCondition(t, sess, src)

	snap, err = sess.Resume(t.Context(), session.ExecContinue, 0, time.Minute, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 0 {
		t.Fatalf("after continue: %+v, want exited with code 0", snap.Session)
	}

	if out := joinOutput(sess.Output(0, 0)); !strings.Contains(out, "done eyedbg 6") {
		t.Errorf("output = %q, want it to contain %q", out, "done eyedbg 6")
	}
}

// stepOverAdd steps over "total += i" (i = 1): only total changed, from 0.
func stepOverAdd(t *testing.T, sess *session.Session) {
	t.Helper()

	snap, err := sess.Resume(t.Context(), session.ExecNext, 0, time.Minute, api.DumpSpec{Dump: []string{api.DumpChanged}})
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

	snap, err := sess.RunUntil(t.Context(), api.BreakpointSpec{File: src, Line: 6, Condition: "i == 3"}, 0, time.Minute, api.DumpSpec{})
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

	if bps := sess.Breakpoints(); len(bps) != 0 {
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
