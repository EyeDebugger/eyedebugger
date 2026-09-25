// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestPause pauses a running program: netcoredbg stops it (reason pause);
// SharpDbg's pause is refused (UNSUPPORTED_BY_ADAPTER, before the lease or
// an exec event) and the session is still running, and still ends. The
// program first stops at its entry and resumes, so the pause names a
// thread: netcoredbg lists no threads before a first stop, and refuses a
// pause without one (a known gap, not SharpDbg's).
func TestPause(t *testing.T) {
	forEachAdapter(t, pause)
}

func pause(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Program.cs")
	marker := filepath.Join(t.TempDir(), "go")
	m := newManager(t)

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{"wait", marker}, StopOnEntry: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if snap := sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}); snap.Session.State != api.StateStopped {
		t.Fatalf("start: %+v, want stopped at the entry", snap.Session)
	}

	if _, err := sess.Exec(t.Context(), agent, session.ExecContinue, 0); err != nil {
		t.Fatal(err)
	}

	if !adapterTraits(adapter).pause {
		execs := len(execEvents(t, sess))

		if _, err := sess.Exec(t.Context(), agent, session.ExecPause, 0); api.CodeOf(err) != api.CodeUnsupported {
			t.Fatalf("pause under %s = %v, want UNSUPPORTED_BY_ADAPTER", adapter, err)
		}

		if st := sess.Info().State; st != api.StateRunning {
			t.Errorf("state after the refused pause = %s, want running", st)
		}

		if n := len(execEvents(t, sess)); n != execs {
			t.Errorf("the refused pause logged %d exec events", n-execs)
		}

		if _, err := m.Stop(t.Context(), agent, sess.ID); err != nil {
			t.Fatalf("stop after the refused pause: %v", err)
		}

		return
	}

	snap := e2eResume(t, sess, session.ExecPause)
	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil || snap.Session.Stop.Reason != "pause" {
		t.Fatalf("pause = %+v, want stopped (pause)", snap.Session)
	}

	t.Logf("paused at %+v (tick line %d)", snap.Frame, lineOf(t, src, "wait-tick"))

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if snap := e2eResume(t, sess, session.ExecContinue); snap.Session.State != api.StateExited {
		t.Errorf("after continue: %+v, want exited", snap.Session)
	}
}

// execEvents are the session's exec events so far.
func execEvents(t *testing.T, sess *session.Session) []api.Event {
	t.Helper()

	res, err := sess.Events(t.Context(), api.EventsParams{Limit: api.MaxEventsLimit, Kinds: []api.EventKind{api.EventExec}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	return res.Events
}

// TestDebuggerDisplay: SharpDbg shows a [DebuggerDisplay] value and runs a
// LINQ lambda (with side effects allowed); netcoredbg's results for the
// same are logged (a documented gap), not asserted.
func TestDebuggerDisplay(t *testing.T) {
	forEachAdapter(t, debuggerDisplay)
}

func debuggerDisplay(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Display.cs")

	sess, err := newManager(t).Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, Args: []string{"display"}},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: "Console.WriteLine"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", lineOf(t, src, "display"))

	const lambda = "items.Where(x => x > 8).Count()"

	p, pErr := sess.Eval(t.Context(), agent, api.EvalParams{Expression: "p"})
	count, countErr := sess.Eval(t.Context(), agent, api.EvalParams{Expression: lambda, AllowSideEffects: true})

	if !adapterTraits(adapter).display {
		t.Logf("%s: p = %q (%v); %s = %q (%v)", adapter, p.Value, pErr, lambda, count.Value, countErr)

		return
	}

	if pErr != nil || p.Value != "Point(3, 4)" {
		t.Errorf("p = %+v, %v; want Point(3, 4)", p, pErr)
	}

	if countErr != nil || count.Value != "2" {
		t.Errorf("%s = %+v, %v; want 2", lambda, count, countErr)
	}
}
