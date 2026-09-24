// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// lines returns line breakpoint specs, "N" or "N if COND".
func lines(specs ...string) []api.BreakpointSpec {
	out := make([]api.BreakpointSpec, 0, len(specs))

	for _, s := range specs {
		n, cond, _ := strings.Cut(s, " if ")

		line, _ := strconv.Atoi(n)
		out = append(out, api.BreakpointSpec{Line: line, Condition: cond})
	}

	return out
}

func replaceLines(t *testing.T, s *Session, c api.Client, file string, specs []api.BreakpointSpec) []api.Breakpoint {
	t.Helper()

	got, err := s.ReplaceBreakpoints(t.Context(), c, file, specs)
	if err != nil {
		t.Fatalf("%s: replace %v: %v", c.ID, specs, err)
	}

	if len(got) != len(specs) {
		t.Fatalf("replace returned %d breakpoints for %d specs", len(got), len(specs))
	}

	return got
}

func ids(bps []api.Breakpoint) []int {
	out := make([]int, len(bps))
	for i := range bps {
		out[i] = bps[i].ID
	}

	return out
}

// bpEvents renders the breakpoint events after seq since as
// "action:owner:line".
func bpEvents(s *Session, since int) []string {
	var out []string

	events := s.log.query(api.EventsParams{Since: since, Limit: api.MaxEventsLimit, Kinds: []api.EventKind{api.EventBreakpoint}}).Events
	for i := range events {
		e := &events[i]
		out = append(out, e.Action+":"+e.Breakpoint.Owner+":"+strconv.Itoa(e.Breakpoint.RequestedLine))
	}

	return out
}

func startReplaceSession(t *testing.T, drv fakeDriver) (s *Session, file string) {
	t.Helper()

	file = writeProgram(t, 10)
	s = start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Program: file, StopOnEntry: true}})

	return s, s.Program
}

// TestReplaceBreakpoints: matches keep their ids, the rest is added or
// removed in one adapter round trip, with events only for changes.
func TestReplaceBreakpoints(t *testing.T) {
	t.Parallel()

	s, file := startReplaceSession(t, fakeDriver{})

	first := replaceLines(t, s, humanC, file, lines("7", "2", "5"))
	if slices.Contains(ids(first), 0) || first[0].Line != 7 || first[1].Line != 2 || !first[2].Verified {
		t.Fatalf("first replace = %+v, want three verified breakpoints in request order", first)
	}

	if got := adapterBPs(t, s); got != "2,5,7" {
		t.Errorf("adapter holds %q", got)
	}

	since := s.log.latest()
	again := replaceLines(t, s, humanC, file, lines("7", "2", "5"))

	if !slices.Equal(ids(again), ids(first)) || len(bpEvents(s, since)) != 0 {
		t.Errorf("re-send = ids %v, events %v; want ids %v and no events", ids(again), bpEvents(s, since), ids(first))
	}

	changed := replaceLines(t, s, humanC, file, lines("2 if false", "5"))
	if changed[0].ID != first[1].ID || changed[1].ID != first[2].ID || changed[0].Condition != "false" {
		t.Errorf("condition change = %+v, want the same ids", changed)
	}

	if got := bpEvents(s, since); !slices.Equal(got, []string{"removed:human:t:7", "changed:human:t:2"}) {
		t.Errorf("events = %v", got)
	}

	if got := adapterBPs(t, s); got != "2 if false,5" {
		t.Errorf("adapter holds %q", got)
	}
}

// TestReplaceKeepsOthers: other owners' breakpoints stay and share their
// line; temporaries stay; RemoveOwnBreakpoints removes only the caller's.
func TestReplaceKeepsOthers(t *testing.T) {
	t.Parallel()

	s, file := startReplaceSession(t, fakeDriver{})
	agents := addBP(t, s, agentC, file, 3, "")

	s.mu.Lock()
	temp := s.addNewLocked(humanC.ID, api.BreakpointSpec{File: file, Line: 9})
	temp.Temporary = true
	s.mu.Unlock()

	mine := replaceLines(t, s, humanC, file, lines("3 if false", "4"))
	if mine[0].ID == agents.ID {
		t.Fatal("the human's breakpoint took the agent's id")
	}

	if got := adapterBPs(t, s); got != "3,4,9" {
		t.Errorf("adapter holds %q, want line 3 unconditional (the agent's)", got)
	}

	replaceLines(t, s, humanC, file, nil)

	left := s.Breakpoints("")
	if len(left) != 2 || left[0].ID != agents.ID || !left[1].Temporary {
		t.Errorf("after clearing = %+v, want the agent's and the temporary", left)
	}

	mine = replaceLines(t, s, humanC, file, lines("5", "6"))

	removed, err := s.RemoveOwnBreakpoints(t.Context(), humanC, []int{agents.ID, temp.ID, mine[0].ID, 999})
	if err != nil || removed != 1 {
		t.Errorf("RemoveOwnBreakpoints = %d, %v; want only the human's own", removed, err)
	}

	if got := adapterBPs(t, s); got != "3,6,9" {
		t.Errorf("adapter holds %q", got)
	}
}

// TestReplaceRefusals: a spec that can't be placed comes back unverified
// with ID 0, the others are placed.
func TestReplaceRefusals(t *testing.T) {
	t.Parallel()

	noConditions := daptest.DefaultCaps()
	noConditions.SupportsConditionalBreakpoints = false

	tests := []struct {
		name    string
		drv     fakeDriver
		specs   []api.BreakpointSpec
		refused []bool
		message string // of the first refusal
	}{
		{name: "same line twice", specs: lines("3", "3 if false"), refused: []bool{false, true}, message: "same line"},
		{
			name:    "bad hit condition",
			specs:   []api.BreakpointSpec{{Line: 3, HitCondition: "x"}, {Line: 4, HitCondition: ">=2"}},
			refused: []bool{true, false}, message: "invalid hit count",
		},
		{
			name:    "bad log message",
			specs:   []api.BreakpointSpec{{Line: 3, LogMessage: "{"}, {Line: 4, LogMessage: "i={x}"}},
			refused: []bool{true, false}, message: "invalid log message",
		},
		{name: "line 0", specs: lines("0", "2"), refused: []bool{true, false}, message: "invalid breakpoint"},
		{
			name: "conditions unsupported", drv: fakeDriver{opts: daptest.Options{Caps: &noConditions}},
			specs: lines("3 if false", "4"), refused: []bool{true, false}, message: "can't evaluate breakpoint conditions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, file := startReplaceSession(t, tt.drv)
			got := replaceLines(t, s, humanC, file, tt.specs)

			for i, b := range got {
				if refused := b.ID == 0; refused != tt.refused[i] || (refused && (b.Verified || b.Message == "")) {
					t.Errorf("breakpoint %d = %+v, want refused: %v", i, b, tt.refused[i])
				}
			}

			if first := got[slices.Index(tt.refused, true)]; !strings.Contains(first.Message, tt.message) {
				t.Errorf("refusal message = %q, want it to mention %q", first.Message, tt.message)
			}
		})
	}
}

// TestReplaceFiles: a missing file refuses every spec, a deleted file's
// breakpoints can still be cleared, and a moved breakpoint re-sent at its
// placed line keeps its id.
func TestReplaceFiles(t *testing.T) {
	t.Parallel()

	s, file := startReplaceSession(t, fakeDriver{})

	for _, missing := range []string{"", "relative.txt", filepath.Join(filepath.Dir(file), "none.txt")} {
		got := replaceLines(t, s, humanC, missing, lines("1", "2"))
		if got[0].ID != 0 || got[1].ID != 0 || got[0].Message == "" {
			t.Errorf("replace in %q = %+v, want every spec refused", missing, got)
		}
	}

	other := writeProgram(t, 5)
	placed := replaceLines(t, s, humanC, other, lines("3"))

	s.mu.Lock()
	s.findBreakpointLocked(placed[0].ID).Line = 4
	s.mu.Unlock()

	if moved := replaceLines(t, s, humanC, other, lines("4")); moved[0].ID != placed[0].ID {
		t.Errorf("re-sent at the placed line = %+v, want id %d", moved[0], placed[0].ID)
	}

	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}

	replaceLines(t, s, humanC, other, nil)

	if left := s.Breakpoints(humanC.ID); len(left) != 0 {
		t.Errorf("after clearing a deleted file: %+v", left)
	}
}

func TestReplaceFunctionBreakpoints(t *testing.T) {
	t.Parallel()

	s, _ := startReplaceSession(t, fakeDriver{})

	first, err := s.ReplaceFunctionBreakpoints(t.Context(), humanC, []api.BreakpointSpec{{Function: "f3"}, {Function: "f5"}})
	if err != nil || first[0].ID == 0 || first[1].ID == 0 {
		t.Fatalf("replace = %+v, %v", first, err)
	}

	got, err := s.ReplaceFunctionBreakpoints(t.Context(), humanC, []api.BreakpointSpec{
		{Function: "f3", Condition: "false"}, {Function: "f7", HitCondition: "2"}, {Function: ""},
	})
	if err != nil || got[0].ID != first[0].ID || got[1].ID != 0 || !strings.Contains(got[1].Message, "hit counts") || got[2].ID != 0 {
		t.Fatalf("second replace = %+v, %v", got, err)
	}

	if fbps := evalValue(t, s, "$fbps"); fbps != "f3 if false" {
		t.Errorf("adapter holds %q", fbps)
	}
}

func TestReplaceOnExitedSession(t *testing.T) {
	t.Parallel()

	s, file := startReplaceSession(t, fakeDriver{})
	if err := s.Terminate(t.Context(), agentC); err != nil {
		t.Fatal(err)
	}

	_, err := s.ReplaceBreakpoints(t.Context(), humanC, file, lines("3"))
	expectCode(t, err, api.CodeSessionExited)

	_, err = s.ReplaceFunctionBreakpoints(t.Context(), humanC, nil)
	expectCode(t, err, api.CodeSessionExited)

	_, err = s.ReplaceBreakpoints(t.Context(), humanC, "", lines("3"))
	expectCode(t, err, api.CodeSessionExited)
}
