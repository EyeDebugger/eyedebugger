// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// attachManager returns a manager whose fake driver attaches to a program
// of 5 lines (hanging at its end unless fail).
func attachManager(t *testing.T, prog string, fail bool) *Manager {
	t.Helper()

	return newTestManagerWith(t, nil, fakeDriver{attach: daptest.ProgramArgs{Program: prog, Lines: 5, Hang: true, FailAttach: fail}})
}

func attachSelf(t *testing.T, m *Manager, p api.StartParams) (*Session, error) {
	t.Helper()

	p.Lang = "fake"
	p.Attach = &api.AttachSpec{PID: os.Getpid()}

	return m.Start(t.Context(), agentC, p)
}

func outputText(s *Session) string {
	var b strings.Builder

	events := eventsOf(s, api.EventOutput)
	for i := range events {
		b.WriteString(events[i].Text)
	}

	return b.String()
}

func TestAttach(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)

	s, err := attachSelf(t, attachManager(t, prog, false), api.StartParams{
		LeasePolicy: api.LeaseHandoff, Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{})
	expectStopped(t, snap, reasonBreakpoint, 3)

	info := snap.Session
	if info.Mode != api.ModeAttach || info.PID != os.Getpid() || !strings.HasPrefix(info.Program, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("info = %+v, want attached to this process", info)
	}

	if started := eventsOf(s, api.EventStarted); len(started) != 1 || started[0].Action != api.ModeAttach {
		t.Errorf("started events = %+v, want one with action attach", started)
	}

	if err := s.Detach(t.Context(), humanC); api.CodeOf(err) != api.CodeLeaseHeld {
		t.Errorf("detach by a client without the lease = %v, want LEASE_HELD", err)
	}

	if err := s.Detach(t.Context(), agentC); err != nil {
		t.Fatal(err)
	}

	final := s.Info()
	if final.State != api.StateExited || final.EndReason != "detached by agent (the program keeps running)" {
		t.Errorf("after detach: %+v", final)
	}

	if !strings.Contains(outputText(s), "fake: disconnect terminateDebuggee=false") {
		t.Errorf("output %q, want an explicit detach", outputText(s))
	}
}

func TestAttachRefusals(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)

	tests := []struct {
		name     string
		fail     bool
		p        api.StartParams
		pid      int
		wantCode api.Code
		wantText string
	}{
		{name: "adapter fails", fail: true, wantCode: api.CodeAttachFailed, wantText: "0x80070057"},
		{name: "launch options", p: api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"x"}}}, wantCode: api.CodeInvalidRequest, wantText: "--stop-on-entry"},
		{name: "no such process", pid: -1, wantCode: api.CodeInvalidRequest, wantText: "no process"},
		{name: "attach and test", p: api.StartParams{Test: &api.TestSpec{}}, wantCode: api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := attachManager(t, prog, tt.fail)

			p := tt.p
			p.Lang = "fake"
			p.Attach = &api.AttachSpec{PID: os.Getpid()}

			if tt.pid != 0 {
				p.Attach.PID = tt.pid
			}

			_, err := m.Start(t.Context(), agentC, p)

			e, ok := err.(*api.Error) //nolint:errorlint // Start returns *api.Error unwrapped.
			if !ok || e.Code != tt.wantCode || !strings.Contains(e.Message, tt.wantText) {
				t.Fatalf("start = %v, want %s with %q", err, tt.wantCode, tt.wantText)
			}

			if tt.fail && e.Hint != "fake attach hint" {
				t.Errorf("hint = %q, want the driver's", e.Hint)
			}

			if n := len(m.List()); n != 0 {
				t.Errorf("%d sessions left after a failed attach", n)
			}
		})
	}
}

// TestStopDetachesAttached: stop (and StopAll) leave an attached program
// running; detach refuses a launched session.
func TestStopDetachesAttached(t *testing.T) {
	t.Parallel()

	m := attachManager(t, fakeProgram(t), false)

	s, err := attachSelf(t, m, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	info, err := m.Stop(t.Context(), agentC, s.ID)
	if err != nil || info.EndReason != "detached by agent (the program keeps running)" {
		t.Fatalf("stop = %+v, %v", info, err)
	}

	if !strings.Contains(outputText(s), "terminateDebuggee=false") {
		t.Errorf("output %q, want a detach", outputText(s))
	}

	s2, err := attachSelf(t, m, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	if n := m.StopAll(t.Context()); n != 1 {
		t.Fatalf("StopAll = %d", n)
	}

	if got := s2.Info().EndReason; got != "detached by the daemon (the program keeps running)" {
		t.Errorf("end reason = %q", got)
	}

	launched := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	if _, err := m.Detach(t.Context(), agentC, launched.ID); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("detach of a launched session = %v, want INVALID_REQUEST", err)
	}

	if kinds := kindsAndActions(eventsOf(launched, api.EventEnded)); !slices.Equal(kinds, []string(nil)) {
		t.Errorf("a refused detach ended the session: %v", kinds)
	}
}
