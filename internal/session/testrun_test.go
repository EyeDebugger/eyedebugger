// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// testManager returns a manager whose fake driver runs the fake test runner
// in mode (exiting with code) and attaches to a fake program of 5 lines.
func testManager(t *testing.T, prog, mode string, code int) *Manager {
	t.Helper()

	return newTestManagerWith(t, nil, fakeDriver{
		attach: daptest.ProgramArgs{Program: prog, Lines: 5}, runner: mode, runnerCode: code,
	})
}

func startTestRun(t *testing.T, m *Manager, p api.StartParams) (*Session, error) {
	t.Helper()

	p.Lang = "fake"
	p.Test = &api.TestSpec{Filter: "Adds"}

	return m.Start(t.Context(), agentC, p)
}

// waitExited waits for the session to end and returns its info.
func waitExited(t *testing.T, s *Session) api.SessionInfo {
	t.Helper()

	snap := s.Wait(t.Context(), 1<<30, testWait, api.DumpSpec{})
	if snap.Session.State != api.StateExited {
		t.Fatalf("session = %+v, want exited", snap.Session)
	}

	return snap.Session
}

func TestTestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode string
		code int
	}{
		{name: "passes", mode: daptest.RunnerExit},
		{name: "fails", mode: daptest.RunnerExit, code: 1},
		{name: "two hosts", mode: daptest.RunnerTwice},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prog := fakeProgram(t)

			s, err := startTestRun(t, testManager(t, prog, tt.mode, tt.code), api.StartParams{
				Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
			})
			if err != nil {
				t.Fatal(err)
			}

			expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 3)

			if info := s.Info(); info.Mode != api.ModeTest || info.Program != "fake test Adds" || info.PID == 0 {
				t.Errorf("info = %+v, want a test session attached to the host", info)
			}

			resume(t, s, agentC, ExecContinue)

			info := waitExited(t, s)
			if info.ExitCode == nil || *info.ExitCode != tt.code || !strings.HasPrefix(info.EndReason, "the test run finished") {
				t.Fatalf("ended = %+v (code %v), want the runner's code %d", info, info.ExitCode, tt.code)
			}

			checkTestRunLog(t, s, tt.mode == daptest.RunnerTwice)
		})
	}
}

// checkTestRunLog: the runner's output is the session's, the host's end is
// noted, and the log ends with the runner's exit.
func checkTestRunLog(t *testing.T, s *Session, twoHosts bool) {
	t.Helper()

	out := outputText(s)
	for _, want := range []string{"Process Id: ", "Passed!", "the test host exited with code 0", "terminateDebuggee=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	if got := strings.Contains(out, "is not debugged"); got != twoHosts {
		t.Errorf("second-host warning = %v, want %v", got, twoHosts)
	}

	all := kindsAndActions(eventsOf(s))
	if !slices.Equal(all[:1], []string{"started:test"}) || !slices.Equal(all[len(all)-2:], []string{"exited", "ended"}) {
		t.Errorf("events = %v, want started:test ... exited, ended", all)
	}

	if n := len(eventsOf(s, api.EventExited)); n != 1 {
		t.Errorf("%d exited events, want only the runner's", n)
	}
}

func TestTestRunFailsWithoutHost(t *testing.T) {
	t.Parallel()

	m := testManager(t, fakeProgram(t), daptest.RunnerNoPID, 1)

	_, err := startTestRun(t, m, api.StartParams{})
	if api.CodeOf(err) != api.CodeBuildFailed || !strings.Contains(err.Error(), "error CS1002") || !strings.Contains(err.Error(), "code 1") {
		t.Fatalf("start = %v, want the driver's failure with the output", err)
	}

	if n := len(m.List()); n != 0 {
		t.Errorf("%d sessions left", n)
	}
}

func TestStopEndsTestRun(t *testing.T) {
	t.Parallel()

	m := testManager(t, fakeProgram(t), daptest.RunnerHang, 0)

	s, err := startTestRun(t, m, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Detach(t.Context(), agentC, s.ID); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("detach of a test run = %v, want INVALID_REQUEST", err)
	}

	info, err := m.Stop(t.Context(), agentC, s.ID)
	if err != nil || info.EndReason != "stopped by agent" {
		t.Fatalf("stop = %+v, %v", info, err)
	}

	// The runner was killed: its exit code arrives (the session is over
	// already, so it is only recorded on the run).
	<-s.run.done

	s.mu.Lock()
	code := *s.run.code
	s.mu.Unlock()

	if code == 0 {
		t.Errorf("runner exit code = 0, want killed")
	}
}

// TestStopWhileWaitingForHost: a client stops a test session before its
// host appeared; the start returns an error and the runner is killed.
func TestStopWhileWaitingForHost(t *testing.T) {
	t.Parallel()

	// The runner prints a pid line, but this driver never recognizes it,
	// so the start keeps waiting.
	drv := blindTester{fakeDriver{attach: daptest.ProgramArgs{Program: fakeProgram(t), Lines: 5}, runner: daptest.RunnerHang}}
	live := make(chan int, 100)

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{
		Drivers: []Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard,
		OnLive: func(n int) {
			select {
			case live <- n:
			default:
			}
		},
	})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	done := make(chan error, 1)

	go func() {
		_, err := startTestRun(t, m, api.StartParams{})
		done <- err
	}()

	for n := range live {
		if n > 0 {
			break
		}
	}

	s, err := m.Get("")
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the runner printed its (unrecognized) pid line.
	if _, err := s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{api.EventOutput}}, testWait); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
		t.Fatal(err)
	}

	if err := <-done; api.CodeOf(err) != api.CodeSessionExited || !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("start = %v, want stopped while starting", err)
	}

	<-s.run.done
}

// blindTester is a fake driver that never finds the test host.
type blindTester struct{ fakeDriver }

func (b blindTester) TestCommand(ctx context.Context, spec TestSpec) (TestCommand, error) {
	tc, err := b.fakeDriver.TestCommand(ctx, spec)
	tc.HostPID = func(string) (int, bool) { return 0, false }

	return tc, err
}

// TestStopAfterRunDoesNotKill: stopping a test session whose runner has
// exited sends no kill: the runner's pid may belong to another process.
func TestStopAfterRunDoesNotKill(t *testing.T) {
	t.Parallel()

	m := testManager(t, fakeProgram(t), daptest.RunnerExit, 0)

	s, err := startTestRun(t, m, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	waitExited(t, s)
	<-s.run.done

	if s.killRunner(t.Context()) {
		t.Fatal("killRunner sent a kill to a runner that had exited")
	}

	if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
		t.Fatal(err)
	}
}
