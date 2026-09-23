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

const program = `var total = 0;
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
// locals, eval, stepping, output and exit.
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
		Breakpoints: []api.BreakpointSpec{{File: src, Line: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap := sess.Wait(t.Context(), 0, time.Minute)
	expectStop(t, snap, "breakpoint", 4)

	scopes, err := sess.Vars(t.Context(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	if got := varValue(scopes, "i"); got != "1" {
		t.Errorf("i = %q, want 1", got)
	}

	res, err := sess.Eval(t.Context(), "total + 41", 0)
	if err != nil || res.Value != "41" {
		t.Errorf("eval total + 41 = %+v, %v; want 41", res, err)
	}

	snap, err = sess.Resume(t.Context(), session.ExecNext, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, snap, "step", 5)

	if _, err := sess.RemoveBreakpoint(t.Context(), 0); err != nil {
		t.Fatal(err)
	}

	snap, err = sess.Resume(t.Context(), session.ExecContinue, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 0 {
		t.Fatalf("after continue: %+v, want exited with code 0", snap.Session)
	}

	var out strings.Builder
	for _, l := range sess.Output(0, 0) {
		out.WriteString(l.Text)
	}

	if !strings.Contains(out.String(), "done eyedbg 6") {
		t.Errorf("output = %q, want it to contain %q", out.String(), "done eyedbg 6")
	}
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
