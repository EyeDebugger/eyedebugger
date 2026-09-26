// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestExitCodeUnknownStart: a driver marking its Launch ExitCodeUnknown
// (e.g. the .NET driver's netcoredbg-on-macOS case) makes a plain launch
// end with no exit code and an end reason explaining why.
func TestExitCodeUnknownStart(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)
	m := newTestManagerWith(t, nil, fakeDriver{knobs: Launch{ExitCodeUnknown: "why"}})

	s := start(t, m, agentC, api.StartParams{
		LaunchSpec:  api.LaunchSpec{Program: prog, Args: []string{"lines=5", "exitCode=3"}},
		Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
	})

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 3)

	snap := resume(t, s, agentC, ExecContinue)
	if snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want exited", snap.Session)
	}

	info := snap.Session
	if info.ExitCode != nil {
		t.Errorf("exit code = %d, want none", *info.ExitCode)
	}

	if !strings.Contains(info.EndReason, "exit code is unknown (why)") {
		t.Errorf("end reason = %q, want it to contain %q", info.EndReason, "exit code is unknown (why)")
	}

	exited := eventsOf(s, api.EventExited)
	if len(exited) != 1 {
		t.Fatalf("%d exited events, want exactly one", len(exited))
	}

	if exited[0].ExitCode != nil {
		t.Errorf("exited event's ExitCode = %d, want none", *exited[0].ExitCode)
	}
}

// TestExitCodeUnknownLaunchedTestRun: the same, for a launched test run
// (mode test, no runner): the end reason keeps the "read the test output"
// hint a known exit code gives, without the code.
func TestExitCodeUnknownLaunchedTestRun(t *testing.T) {
	t.Parallel()

	prog := launchedProgram(t, 5)
	m := newTestManagerWith(t, nil, fakeDriver{
		launch: &daptest.ProgramArgs{Program: prog, Lines: 5},
		knobs:  Launch{ExitCodeUnknown: "why"},
	})

	s, err := startTestRun(t, m, api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
		LaunchSpec:  api.LaunchSpec{Args: []string{"exitCode=3"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 3)

	resume(t, s, agentC, ExecContinue)

	info := waitExited(t, s)
	if info.ExitCode != nil {
		t.Errorf("exit code = %d, want none", *info.ExitCode)
	}

	want := "the test run finished; the test app's exit code is unknown (why): read the test output for the result"
	if info.EndReason != want {
		t.Errorf("end reason = %q, want %q", info.EndReason, want)
	}

	if n := len(eventsOf(s, api.EventExited)); n != 1 {
		t.Errorf("%d exited events, want exactly one", n)
	}
}

// TestExitCodeUnknownRunnerHost: a runner-owned test run's own exit code
// (from the runner process, never the adapter) is unaffected; only its
// test host's exit report loses its (untrusted) code.
func TestExitCodeUnknownRunnerHost(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)
	m := newTestManagerWith(t, nil, fakeDriver{
		attach: daptest.ProgramArgs{Program: prog, Lines: 5},
		runner: daptest.RunnerExit, runnerCode: 1,
		knobs: Launch{ExitCodeUnknown: "why"},
	})

	s, err := startTestRun(t, m, api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 3)

	resume(t, s, agentC, ExecContinue)

	info := waitExited(t, s)
	if info.ExitCode == nil || *info.ExitCode != 1 {
		t.Fatalf("ended = %+v, want the runner's exit code 1 (untrusted only affects the adapter's own)", info)
	}

	if !strings.HasPrefix(info.EndReason, "the test run finished") {
		t.Errorf("end reason = %q, want it to start with %q", info.EndReason, "the test run finished")
	}

	out := outputText(s)
	if !strings.Contains(out, "eyedbg: the test host exited") {
		t.Errorf("output lacks %q:\n%s", "eyedbg: the test host exited", out)
	}

	if strings.Contains(out, "with code") {
		t.Errorf("output = %q, want no exit code for the untrusted host", out)
	}
}
