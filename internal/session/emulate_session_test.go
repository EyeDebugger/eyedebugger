// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// lapsSession starts a fake program of 3 lines run 5 times, stopped at
// entry.
func lapsSession(t *testing.T, drv fakeDriver) *Session {
	t.Helper()

	return start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{
		LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"lines=3", "laps=5"}},
	})
}

func addSpec(t *testing.T, s *Session, c api.Client, spec api.BreakpointSpec) api.Breakpoint {
	t.Helper()

	if spec.File == "" && spec.Function == "" {
		spec.File = s.Program
	}

	b, err := s.AddBreakpoint(t.Context(), c, spec)
	if err != nil {
		t.Fatalf("%s: add %+v: %v", c.ID, spec, err)
	}

	return b
}

// lapsOfStops continues until the program exits and returns the lap of
// every stop.
func lapsOfStops(t *testing.T, s *Session) []int {
	t.Helper()

	var laps []int

	for {
		snap := resume(t, s, agentC, ExecContinue)
		if snap.Session.State == api.StateExited {
			return laps
		}

		if snap.Session.State != api.StateStopped || snap.TimedOut {
			t.Fatalf("snapshot = %+v, want stopped or exited", snap.Session)
		}

		n, err := strconv.Atoi(evalValue(t, s, "lap"))
		if err != nil {
			t.Fatal(err)
		}

		laps = append(laps, n)
	}
}

func logpointLines(s *Session) []string {
	var out []string

	events := eventsOf(s, api.EventOutput)
	for i := range events {
		if events[i].Category == logpointCategory {
			out = append(out, events[i].Text)
		}
	}

	return out
}

func TestHitConditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hit  string
		want []int
	}{
		{"3", []int{3}},
		{">=4", []int{4, 5}},
		{"%2", []int{2, 4}},
	}

	for _, tt := range tests {
		t.Run(tt.hit, func(t *testing.T) {
			t.Parallel()

			s := lapsSession(t, fakeDriver{})
			b := addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, HitCondition: tt.hit})

			if got := lapsOfStops(t, s); !slices.Equal(got, tt.want) {
				t.Fatalf("--hit %s stopped at laps %v, want %v", tt.hit, got, tt.want)
			}

			if bps := s.Breakpoints(""); len(bps) != 1 || bps[0].ID != b.ID || bps[0].Hits != 5 {
				t.Errorf("breakpoints = %+v, want %d with 5 hits", bps, b.ID)
			}

			if n := len(eventsOf(s, api.EventContinued)); n != 0 {
				t.Errorf("%d continued events: the filter's own resumes must not be logged", n)
			}
		})
	}
}

func TestLogpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		other     *api.BreakpointSpec // the human's breakpoint at the same line
		wantLaps  []int
		wantLines int
	}{
		{name: "alone", wantLines: 5},
		{name: "with another client's plain breakpoint", other: &api.BreakpointSpec{Line: 2}, wantLaps: []int{1, 2, 3, 4, 5}, wantLines: 5},
		{name: "with another client's false condition", other: &api.BreakpointSpec{Line: 2, Condition: "line == 999"}, wantLines: 5},
		{name: "with another client's condition that holds once", other: &api.BreakpointSpec{Line: 2, Condition: "lap == 4"}, wantLaps: []int{4}, wantLines: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := lapsSession(t, fakeDriver{})
			addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, LogMessage: "at {line} lap {lap}"})

			if tt.other != nil {
				addSpec(t, s, humanC, *tt.other)
			}

			if got := lapsOfStops(t, s); !slices.Equal(got, tt.wantLaps) {
				t.Fatalf("stopped at laps %v, want %v", got, tt.wantLaps)
			}

			lines := logpointLines(s)
			if len(lines) != tt.wantLines || lines[0] != "at 2 lap 1\n" || lines[4] != "at 2 lap 5\n" {
				t.Fatalf("logpoint output = %q, want %d lines 'at 2 lap N'", lines, tt.wantLines)
			}
		})
	}
}

func TestLogpointErrorsAreLogged(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	addSpec(t, s, agentC, api.BreakpointSpec{Line: 1, LogMessage: "v={nope}", HitCondition: "2"})

	if got := lapsOfStops(t, s); len(got) != 0 {
		t.Fatalf("stops at laps %v, want none", got)
	}

	if lines := logpointLines(s); len(lines) != 1 || !strings.HasPrefix(lines[0], "v=<error: ") {
		t.Fatalf("logpoint output = %q, want one error line (hit 2 only)", lines)
	}
}

// TestStepOntoLogpoint: a step that the adapter ends at a breakpoint (as
// netcoredbg does) is published even though the logpoint doesn't stop.
func TestStepOntoLogpoint(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{opts: daptest.Options{StepHitsBreakpoints: true}})
	addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, LogMessage: "here"})

	snap := resume(t, s, agentC, ExecNext)
	expectStopped(t, snap, reasonBreakpoint, 2)

	if snap.Session.Stop.Description != stepEndedDescription {
		t.Errorf("description = %q, want the step note", snap.Session.Stop.Description)
	}

	if lines := logpointLines(s); len(lines) != 1 {
		t.Errorf("logpoint output = %q, want one line", lines)
	}
}

// TestRunUntilOwnLogpoint: run-until to a line where the caller has a
// logpoint places a temporary breakpoint beside it and reaches the line.
func TestRunUntilOwnLogpoint(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	lp := addSpec(t, s, agentC, api.BreakpointSpec{Line: 3, LogMessage: "lp"})

	snap := runUntil(t, s, agentC, 3, testWait)
	expectStopped(t, snap, reasonBreakpoint, 3)

	if snap.Reached == nil || !*snap.Reached {
		t.Fatalf("reached = %v, want true", snap.Reached)
	}

	if bps := s.Breakpoints(""); len(bps) != 1 || bps[0].ID != lp.ID || bps[0].Hits != 1 {
		t.Errorf("breakpoints after run-until = %+v, want the logpoint alone, hit once", bps)
	}
}

// TestReaddResetsHits: re-adding replaces the hit condition and restarts
// the count.
func TestReaddResetsHits(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, HitCondition: "2"})

	expectStopped(t, resume(t, s, agentC, ExecContinue), reasonBreakpoint, 2)

	b := addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, HitCondition: "2"})
	if b.Hits != 0 {
		t.Fatalf("re-added = %+v, want the count restarted", b)
	}

	// Laps 3 (hit 1) and 4 (hit 2: stops).
	expectStopped(t, resume(t, s, agentC, ExecContinue), reasonBreakpoint, 2)

	if got := evalValue(t, s, "lap"); got != "4" {
		t.Errorf("stopped in lap %s, want 4", got)
	}
}

// TestFilterDropsStaleStops: a filter whose stop is no longer the latest
// news (the session exited, or it resumed since) publishes nothing.
func TestFilterDropsStaleStops(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	addSpec(t, s, agentC, api.BreakpointSpec{Line: 2, LogMessage: "x"})

	s.mu.Lock()
	stale := s.stopGen - 1
	s.mu.Unlock()

	stops := s.Stops()
	s.filterStop(stale, api.StopInfo{Reason: reasonBreakpoint, ThreadID: 1})

	if s.Stops() != stops {
		t.Fatal("a stale filter published a stop")
	}

	if got := lapsOfStops(t, s); len(got) != 0 {
		t.Fatalf("stops %v, want none", got)
	}

	s.mu.Lock()
	gen := s.stopGen
	s.mu.Unlock()

	events := len(eventsOf(s))
	s.filterStop(gen, api.StopInfo{Reason: reasonBreakpoint, ThreadID: 1})

	if n := len(eventsOf(s)); n != events {
		t.Errorf("a filter after exit logged %d events", n-events)
	}
}

// TestPauseDuringFilterPublishes: a pause that reached the adapter while
// the filter held a stop did nothing there; the filter publishes that stop
// instead of continuing past it.
func TestPauseDuringFilterPublishes(t *testing.T) {
	t.Parallel()

	s := lapsSession(t, fakeDriver{})
	addSpec(t, s, agentC, api.BreakpointSpec{Line: 1, LogMessage: "here"})

	// The program is stopped at entry, line 1: as when the filter holds a
	// breakpoint stop there, with a pause sent meanwhile.
	s.mu.Lock()
	gen := s.stopGen
	s.pauseRequested = true
	s.mu.Unlock()

	stops := s.Stops()
	s.filterStop(gen, api.StopInfo{Reason: reasonBreakpoint, ThreadID: 1})

	info := s.Info()
	if s.Stops() != stops+1 || info.Stop == nil || info.Stop.Description != pausedDescription {
		t.Fatalf("after the filter: %d stops (want %d), stop %+v; want the stop published as paused", s.Stops(), stops+1, info.Stop)
	}

	s.mu.Lock()
	still := s.pauseRequested
	s.mu.Unlock()

	if still {
		t.Error("pauseRequested still set after the stop was applied")
	}

	if lines := logpointLines(s); len(lines) != 1 {
		t.Errorf("logpoint output = %q, want it logged once", lines)
	}
}

// stopsFor continues until the program exits and returns the lap of every
// stop and the owners of the breakpoints it was for (nil: not attributed).
func stopsFor(t *testing.T, s *Session) (laps []int, owners [][]string) {
	t.Helper()

	for {
		snap := resume(t, s, agentC, ExecContinue)
		if snap.Session.State == api.StateExited {
			return laps, owners
		}

		if snap.Session.State != api.StateStopped || snap.TimedOut || snap.Session.Stop == nil {
			t.Fatalf("snapshot = %+v, want stopped or exited", snap.Session)
		}

		n, err := strconv.Atoi(evalValue(t, s, "lap"))
		if err != nil {
			t.Fatal(err)
		}

		laps = append(laps, n)
		owners = append(owners, ownersOf(snap.Session.Stop.Breakpoints))
	}
}

// ownersOf returns the owners of an attribution (nil for none).
func ownersOf(bps []api.StopBreakpoint) []string {
	var who []string
	for _, b := range bps {
		who = append(who, b.Owner)
	}

	return who
}

// breakpointStopOwners returns ownersOf of every logged breakpoint stop.
func breakpointStopOwners(s *Session) [][]string {
	var out [][]string

	stops := eventsOf(s, api.EventStopped)
	for i := range stops {
		if stops[i].Stop.Reason == reasonBreakpoint {
			out = append(out, ownersOf(stops[i].Stop.Breakpoints))
		}
	}

	return out
}

// checkShared checks what a shared-line run left: logpoint lines, no
// logged continued events, no notes, the human's hit count (when wantHits
// is set) and the stopped events' attribution.
func checkShared(t *testing.T, s *Session, wantFor [][]string, wantLines, wantHits int) {
	t.Helper()

	if n := len(logpointLines(s)); n != wantLines {
		t.Errorf("%d logpoint lines, want %d", n, wantLines)
	}

	if n := len(eventsOf(s, api.EventContinued)); n != 0 {
		t.Errorf("%d continued events: the filter's own resumes must not be logged", n)
	}

	bps := s.Breakpoints("")
	for i := range bps {
		if bps[i].Note != "" {
			t.Errorf("breakpoint %d: note %q, want none", bps[i].ID, bps[i].Note)
		}

		if bps[i].Owner == humanC.ID && wantHits > 0 && bps[i].Hits != wantHits {
			t.Errorf("human's breakpoint: %d hits, want %d", bps[i].Hits, wantHits)
		}
	}

	// The events' stops carry the same attribution as the snapshots.
	if fromEvents := breakpointStopOwners(s); !reflect.DeepEqual(fromEvents, wantFor) {
		t.Errorf("stopped events for %q, want %q", fromEvents, wantFor)
	}
}

// TestSharedConditions: each client's breakpoint at a shared line keeps its
// own condition, hit count and log message; the program stops when any
// wants it, and the stop names whose breakpoints it is for.
func TestSharedConditions(t *testing.T) {
	t.Parallel()

	logC := api.Client{ID: "agent:log", Kind: api.KindAgent}

	type bp struct {
		c    api.Client
		spec api.BreakpointSpec
	}

	var (
		agent = func(cond string) bp { return bp{agentC, api.BreakpointSpec{Line: 2, Condition: cond}} }
		human = func(cond string) bp { return bp{humanC, api.BreakpointSpec{Line: 2, Condition: cond}} }
		a, h  = []string{agentC.ID}, []string{humanC.ID}
		both  = []string{agentC.ID, humanC.ID}
	)

	tests := []struct {
		name      string
		bps       []bp
		wantLaps  []int
		wantFor   [][]string
		wantLines int // logpoint lines
		wantHits  int // the human's breakpoint's hit count
	}{
		{
			name: "different conditions", bps: []bp{agent("lap == 2"), human("lap == 4")},
			wantLaps: []int{2, 4}, wantFor: [][]string{a, h},
		},
		{
			name: "equal conditions: the adapter decides", bps: []bp{agent("lap == 2"), human("lap == 2")},
			wantLaps: []int{2}, wantFor: [][]string{nil},
		},
		{
			name: "conditional and unconditional", bps: []bp{agent("lap == 2"), human("")},
			wantLaps: []int{1, 2, 3, 4, 5}, wantFor: [][]string{h, both, h, h, h},
		},
		{
			name:     "another's logpoint suppresses nothing",
			bps:      []bp{agent("lap == 2"), human("lap == 4"), {logC, api.BreakpointSpec{Line: 2, LogMessage: "lap {lap}"}}},
			wantLaps: []int{2, 4}, wantFor: [][]string{a, h}, wantLines: 5,
		},
		{
			name: "a condition that fails to evaluate stops", bps: []bp{agent("lap == 2"), human("nosuch")},
			wantLaps: []int{1, 2, 3, 4, 5}, wantFor: [][]string{h, both, h, h, h},
		},
		{
			name: "hit count and condition", bps: []bp{{humanC, api.BreakpointSpec{Line: 2, HitCondition: "2"}}, agent("lap == 4")},
			wantLaps: []int{2, 4}, wantFor: [][]string{h, a}, wantHits: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := lapsSession(t, fakeDriver{})
			for _, b := range tt.bps {
				addSpec(t, s, b.c, b.spec)
			}

			laps, owners := stopsFor(t, s)
			if !slices.Equal(laps, tt.wantLaps) || !reflect.DeepEqual(owners, tt.wantFor) {
				t.Fatalf("stopped at laps %v for %q, want %v for %q", laps, owners, tt.wantLaps, tt.wantFor)
			}

			checkShared(t, s, tt.wantFor, tt.wantLines, tt.wantHits)
		})
	}
}
