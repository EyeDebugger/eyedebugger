// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestDebugTests debugs a 'dotnet test' run: a breakpoint in the passing
// test stops it, and each run ends with dotnet test's exit code.
func TestDebugTests(t *testing.T) {
	requireE2E(t)

	dir := copyApp(t, "tests")
	src := filepath.Join(dir, "CalculatorTests.cs")
	m := newManager(t)

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", LaunchSpec: api.LaunchSpec{Project: dir}, Test: &api.TestSpec{Filter: "Adds"},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: "Assert.Equal(5, sum);"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", lineOf(t, src, "adds-assert"))

	if got := e2eEval(t, sess, "a + b"); got != "5" {
		t.Errorf("a + b = %s, want 5", got)
	}

	snap := e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 0 {
		t.Fatalf("after continue: %+v, want the run exited 0", snap.Session)
	}

	if out := joinOutput(sess.Output(0, 0).Lines); !strings.Contains(out, "Passed!") {
		t.Errorf("output lacks Passed!:\n%s", out)
	}

	sess, err = m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", LaunchSpec: api.LaunchSpec{Project: dir, NoBuild: true}, Test: &api.TestSpec{Filter: "Fails"},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap = sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 1 {
		t.Fatalf("failing run: %+v, want exited 1", snap.Session)
	}
}
