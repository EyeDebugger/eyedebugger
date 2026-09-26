// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strconv"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func runUntil(t *testing.T, s *Session, c api.Client, line int, wait time.Duration) api.Snapshot {
	t.Helper()

	snap, err := s.RunUntil(t.Context(), c, api.BreakpointSpec{File: s.Program, Line: line}, 0, wait, api.DumpSpec{})
	if err != nil {
		t.Fatalf("%s run-until %d: %v", c.ID, line, err)
	}

	return snap
}

func temporaries(s *Session) []api.Breakpoint {
	var out []api.Breakpoint

	all := s.Breakpoints("")
	for i := range all {
		if all[i].Temporary {
			out = append(out, all[i])
		}
	}

	return out
}

// TestRunUntilOwnsItsBreakpoint: run-until reuses only the caller's own
// breakpoint at the target, and removes only its temporary one.
func TestRunUntilOwnsItsBreakpoint(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	// The human's conditional breakpoint at 5 never stops; the agent's
	// run-until adds its own there, which does.
	h5 := addBP(t, s, humanC, s.Program, 5, "false")

	snap := runUntil(t, s, agentC, 5, testWait)
	expectStopped(t, snap, "breakpoint", 5)

	if snap.Reached == nil || !*snap.Reached {
		t.Errorf("reached = %v, want true", snap.Reached)
	}

	if bps := s.Breakpoints(""); len(bps) != 1 || bps[0].ID != h5.ID {
		t.Errorf("breakpoints after run-until = %+v, want only the human's %d", bps, h5.ID)
	}

	if got := adapterBPs(t, s); got != "5 if false" {
		t.Errorf("adapter breakpoints = %q, want the human's alone", got)
	}

	// The agent's own breakpoint at 7 is reused, condition and all: the
	// program never stops there and exits.
	a7 := addBP(t, s, agentC, s.Program, 7, "false")

	snap = runUntil(t, s, agentC, 7, testWait)
	if snap.Session.State != api.StateExited || snap.Reached == nil || *snap.Reached {
		t.Fatalf("run-until to a never-true own breakpoint: %+v reached %v, want exited, not reached", snap.Session, snap.Reached)
	}

	if bps := s.Breakpoints(agentC.ID); len(bps) != 1 || bps[0].ID != a7.ID || bps[0].Temporary {
		t.Errorf("agent's breakpoints = %+v, want %d kept as is", bps, a7.ID)
	}
}

// TestRunUntilKeepsABreakpointMadePermanent: while a run-until waits, its
// caller adds a breakpoint at the same line, which makes the temporary one
// permanent; the run-until's cleanup must leave it.
func TestRunUntilKeepsABreakpointMadePermanent(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"hang"}}})
	since := s.log.latest()

	done := make(chan api.Snapshot, 1)

	go func() { done <- runUntil(t, s, agentC, 12, testWait) }() // past the end: never reached

	// The temporary breakpoint is logged once placed, before the send.
	res, err := s.Events(t.Context(), api.EventsParams{Since: since, Kinds: []api.EventKind{api.EventBreakpoint}}, testWait)
	if err != nil || len(res.Events) != 1 || !res.Events[0].Breakpoint.Temporary {
		t.Fatalf("waiting for the temporary breakpoint: %+v, %v", res, err)
	}

	b := addBP(t, s, agentC, s.Program, 12, "")
	if b.ID != res.Events[0].Breakpoint.ID || b.Temporary {
		t.Fatalf("bp add at the target = %+v, want the temporary one made permanent", b)
	}

	// The pause queues behind the run-until's send, then ends its wait.
	expectStopped(t, resume(t, s, humanC, ExecPause), "pause", 10)

	if snap := <-done; snap.TimedOut || snap.Reached == nil || *snap.Reached {
		t.Fatalf("run-until = timed out %v, reached %v; want stopped elsewhere", snap.TimedOut, snap.Reached)
	}

	if bps := s.Breakpoints(agentC.ID); len(bps) != 1 || bps[0].ID != b.ID || bps[0].Temporary {
		t.Errorf("agent's breakpoints after the run-until = %+v, want %d kept", bps, b.ID)
	}
}

// TestNextExecutionRemovesTimedOutRunUntil: a run-until that timed out
// leaves its breakpoint, and any client's next execution command removes it.
func TestNextExecutionRemovesTimedOutRunUntil(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"hang"}}})

	if snap := runUntil(t, s, agentC, 12, 30*time.Millisecond); !snap.TimedOut {
		t.Fatalf("run-until past the end: %+v, want a timeout", snap.Session)
	}

	temp := temporaries(s)
	if len(temp) != 1 || temp[0].Owner != agentC.ID {
		t.Fatalf("temporaries after a timeout = %+v, want the agent's", temp)
	}

	expectStopped(t, resume(t, s, humanC, ExecPause), "pause", 10)

	if len(temporaries(s)) != 1 {
		t.Fatal("pause removed the temporary breakpoint; only a resume should")
	}

	since := s.log.latest()

	resume(t, s, humanC, ExecNext)

	if left := temporaries(s); len(left) != 0 {
		t.Errorf("temporaries after the human's next = %+v, want none", left)
	}

	removed := s.log.query(api.EventsParams{Since: since, Kinds: []api.EventKind{api.EventBreakpoint}}).Events
	if len(removed) != 1 || removed[0].Action != "removed" || removed[0].Client != humanC.ID || removed[0].Breakpoint.Owner != agentC.ID {
		t.Errorf("breakpoint events = %+v, want the human removing the agent's temporary one", removed)
	}
}

// TestRunUntilSharedLine: a run-until to a line where another client's
// breakpoint stops first is not reached there; the next one is.
func TestRunUntilSharedLine(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	h := addSpec(t, s, humanC, api.BreakpointSpec{Line: 2, Condition: "lap == 2"})

	until := func() (api.Snapshot, int) {
		t.Helper()

		snap, err := s.RunUntil(t.Context(), agentC, api.BreakpointSpec{File: s.Program, Line: 2, Condition: "lap == 3"}, 0, testWait, api.DumpSpec{})
		if err != nil {
			t.Fatalf("run-until: %v", err)
		}

		expectStopped(t, snap, "breakpoint", 2)

		lap, err := strconv.Atoi(evalValue(t, s, "lap"))
		if err != nil {
			t.Fatal(err)
		}

		return snap, lap
	}

	snap, lap := until()
	if st := snap.Session.Stop; lap != 2 || snap.Reached == nil || *snap.Reached ||
		len(st.Breakpoints) != 1 || st.Breakpoints[0].ID != h.ID {
		t.Fatalf("first run-until: lap %d, reached %v, for %+v; want lap 2, not reached, for the human's %d", lap, snap.Reached, st.Breakpoints, h.ID)
	}

	snap, lap = until()
	if st := snap.Session.Stop; lap != 3 || snap.Reached == nil || !*snap.Reached ||
		len(st.Breakpoints) != 1 || st.Breakpoints[0].Owner != agentC.ID || st.Breakpoints[0].ID == h.ID {
		t.Fatalf("second run-until: lap %d, reached %v, for %+v; want lap 3, reached, for the agent's temporary one", lap, snap.Reached, st.Breakpoints)
	}

	if len(temporaries(s)) != 0 {
		t.Errorf("temporaries left: %+v", temporaries(s))
	}
}
