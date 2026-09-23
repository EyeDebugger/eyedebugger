// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

const renderBase = "/work/app"

func stoppedSnapshot() api.Snapshot {
	return api.Snapshot{
		Session: api.SessionInfo{
			ID: "s-k3f9", Lang: "dotnet", Program: "/work/app/bin/Debug/net10.0/app.dll", State: api.StateStopped,
			PID: 4242, CreatedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
			Stop: &api.StopInfo{Reason: "breakpoint", ThreadID: 4242},
		},
		Frame: &api.Frame{Name: "Program.<Main>$()", File: "/work/app/Program.cs", Line: 4, Column: 5},
		Source: []api.SourceLine{
			{Line: 3, Text: "{"},
			{Line: 4, Text: "    total += i;", Current: true},
			{Line: 5, Text: "    Console.WriteLine(total);"},
		},
	}
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
		{ID: 1, File: "/work/app/Program.cs", RequestedLine: 4, Line: 4, Verified: true},
		{ID: 2, File: "/other/Lib.cs", RequestedLine: 10, Line: 12, Verified: true},
		{ID: 3, File: "/work/app/Late.cs", RequestedLine: 7, Line: 7, Message: "pending until the module loads"},
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
		{"output", "output.golden", func(b *bytes.Buffer) error {
			return writeOutput(b, []api.OutputLine{{Seq: 1, Category: "stdout", Text: "i=1\n"}, {Seq: 2, Category: "stderr", Text: "warn"}}, false)
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

func TestParseLocation(t *testing.T) {
	t.Parallel()

	spec, err := parseLocation("/src/Program.cs:12")
	if err != nil || spec.File != "/src/Program.cs" || spec.Line != 12 {
		t.Errorf("parseLocation = %+v, %v", spec, err)
	}

	for _, bad := range []string{"Program.cs", "Program.cs:0", "Program.cs:x", ":3"} {
		if _, err := parseLocation(bad); err == nil {
			t.Errorf("parseLocation(%q) succeeded, want an error", bad)
		}
	}
}
