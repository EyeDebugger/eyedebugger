// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

func addFunc(t *testing.T, s *Session, c api.Client, name string) api.Breakpoint {
	t.Helper()

	b, err := s.AddBreakpoint(t.Context(), c, api.BreakpointSpec{Function: name})
	if err != nil {
		t.Fatalf("%s: add function breakpoint %s: %v", c.ID, name, err)
	}

	return b
}

// TestFunctionBreakpoints: function breakpoints share one slot per name,
// stop the program, and leave line breakpoints alone.
func TestFunctionBreakpoints(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	a := addFunc(t, s, agentC, "f4")
	if !a.Verified || a.Function != "f4" || a.File != "" {
		t.Fatalf("function breakpoint = %+v, want verified f4 without a file", a)
	}

	h := addFunc(t, s, humanC, "f4")
	addBP(t, s, humanC, s.Program, 4, "")

	if bad := addFunc(t, s, agentC, "nope"); bad.Verified || !strings.Contains(bad.Message, "no function") {
		t.Errorf("unknown function = %+v, want unverified", bad)
	}

	if got := evalValue(t, s, "$fbps"); got != "nope,f4" {
		t.Errorf("adapter function breakpoints = %q, want one f4 slot (and nope)", got)
	}

	if got := adapterBPs(t, s); got != "4" {
		t.Errorf("adapter line breakpoints = %q, want 4", got)
	}

	expectStopped(t, resume(t, s, agentC, ExecContinue), "breakpoint", 4)

	if _, _, err := s.RemoveBreakpoint(t.Context(), agentC, 0, false); err != nil {
		t.Fatal(err)
	}

	if got := evalValue(t, s, "$fbps"); got != "f4" {
		t.Errorf("after the agent removed its own = %q, want the human's f4", got)
	}

	if _, _, err := s.RemoveBreakpoint(t.Context(), humanC, h.ID, false); err != nil {
		t.Fatal(err)
	}

	if got := evalValue(t, s, "$fbps"); got != "" {
		t.Errorf("after removing all = %q, want none", got)
	}
}

func TestBreakpointKindsAreChecked(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	tests := []struct {
		name string
		spec api.BreakpointSpec
		code api.Code
	}{
		{"function with a line", api.BreakpointSpec{Function: "f2", File: s.Program, Line: 2}, api.CodeInvalidRequest},
		{"function with a hit count", api.BreakpointSpec{Function: "f2", HitCondition: "2"}, api.CodeInvalidRequest},
		{"function with a log", api.BreakpointSpec{Function: "f2", LogMessage: "x"}, api.CodeInvalidRequest},
		{"relative file", api.BreakpointSpec{File: "prog.txt", Line: 2}, api.CodeInvalidRequest},
		{"no line", api.BreakpointSpec{File: s.Program}, api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		_, err := s.AddBreakpoint(t.Context(), agentC, tt.spec)
		if api.CodeOf(err) != tt.code {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.code)
		}
	}

	_, err := s.RunUntil(t.Context(), agentC, api.BreakpointSpec{Function: "f2"}, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeInvalidRequest)
}

// TestUnsupportedByAdapter: what the adapter doesn't declare is refused.
func TestUnsupportedByAdapter(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{opts: daptest.Options{Caps: &godap.Capabilities{SupportsConfigurationDoneRequest: true}}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	_, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{Function: "f2"})
	expectCode(t, err, api.CodeUnsupported)

	_, err = s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: s.Program, Line: 2, Condition: "true"})
	expectCode(t, err, api.CodeUnsupported)

	_, err = s.Exceptions(t.Context(), agentC, api.ExceptionsParams{Mode: api.ExceptionsAll})
	expectCode(t, err, api.CodeUnsupported)

	if bps := s.Breakpoints(""); len(bps) != 0 {
		t.Errorf("refused breakpoints were kept: %+v", bps)
	}

	// A plain one still works.
	addBP(t, s, agentC, s.Program, 2, "")
}

// TestAnchorBreakpoints: an anchor resolves to its line when added, at
// start too; a file changed after the start gets a note.
func TestAnchorBreakpoints(t *testing.T) {
	t.Parallel()

	prog := writeProgram(t, 10, "", "", "", "", "", "total += price")
	m := newTestManager(t, nil)

	s := start(t, m, agentC, api.StartParams{
		LaunchSpec:  api.LaunchSpec{Program: prog},
		Breakpoints: []api.BreakpointSpec{{File: prog, Anchor: "line 3"}},
		Wait:        api.Duration(testWait),
	})
	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "breakpoint", 3)

	b, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: prog, Anchor: "total  +=  price"})
	if err != nil || b.Line != 6 || b.Anchor != "total  +=  price" || b.Note != "" {
		t.Fatalf("anchor breakpoint = %+v, %v; want line 6 with its anchor and no note", b, err)
	}

	_, err = s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: prog, Anchor: "line"})
	expectCode(t, err, api.CodeAnchorAmbiguous)

	_, err = s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: prog, Anchor: "total -= price"})
	expectCode(t, err, api.CodeAnchorNotFound)

	_, err = m.Start(t.Context(), agentC, api.StartParams{
		Lang: "fake", LaunchSpec: api.LaunchSpec{Program: prog},
		Breakpoints: []api.BreakpointSpec{{File: prog, Anchor: "nowhere"}},
	})
	expectCode(t, err, api.CodeAnchorNotFound)

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(prog, future, future); err != nil {
		t.Fatal(err)
	}

	for _, b := range s.Breakpoints("") {
		if !strings.Contains(b.Note, "prog.txt changed after the session started") {
			t.Errorf("breakpoint %d note = %q, want the stale note", b.ID, b.Note)
		}
	}
}
