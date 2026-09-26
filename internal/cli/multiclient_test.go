// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// isolate points the CLI at a fresh runtime directory and home ($EYEDBG_HOME),
// with no auto-start and no inherited identity or session. Not for parallel
// tests.
func isolate(t *testing.T) daemon.Paths {
	t.Helper()

	dir, err := os.MkdirTemp("", "edc") //nolint:usetesting // t.TempDir paths are too long for a Unix socket on macOS.
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	rt := filepath.Join(dir, "rt")
	t.Setenv(daemon.EnvRuntimeDir, rt)
	t.Setenv(daemon.EnvNoAutostart, "1")
	t.Setenv(envClient, "")
	t.Setenv(envSession, "")
	t.Setenv(envNoRecord, "")
	// Dumps go under the test's directory, never the real ~/.eyedbg.
	t.Setenv(adapters.EnvHome, filepath.Join(dir, "home"))

	return daemon.PathsIn(rt)
}

// serveInProcess runs a daemon with the fake driver until the test ends.
func serveInProcess(t *testing.T, p daemon.Paths) {
	t.Helper()

	serveWithDrivers(t, p, fakeDriver{})
}

// serveWithDrivers runs a daemon with drivers until the test ends.
func serveWithDrivers(t *testing.T, p daemon.Paths, drivers ...session.Driver) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- daemon.Serve(ctx, daemon.Config{
			Paths: p, IdleTimeout: time.Hour, Info: testInfo, Logger: slog.New(slog.DiscardHandler),
			Drivers: drivers,
		})
	}()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	})

	waitForDaemon(t, p, done)
}

// waitForDaemon waits until the daemon serving p (done receives Serve's
// result) accepts connections.
func waitForDaemon(t *testing.T, p daemon.Paths, done <-chan error) {
	t.Helper()

	ctx2, cancel2 := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel2()

	for {
		cl, err := daemon.Dial(ctx2, p, testInfo)
		if err == nil {
			_ = cl.Close()

			return
		}

		select {
		case err := <-done:
			t.Fatalf("Serve returned early: %v", err)
		case <-ctx2.Done():
			t.Fatalf("daemon never accepted connections: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// run executes eyedbg with args and checks its exit code; it returns stdout
// and stderr together.
func run(t *testing.T, wantCode int, args ...string) string {
	t.Helper()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), args)
	if code != wantCode {
		t.Fatalf("eyedbg %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, wantCode, stdout, stderr)
	}

	return string(stdout) + string(stderr)
}

func expectOutput(t *testing.T, out string, want ...string) {
	t.Helper()

	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// TestMultiClientCLI drives one session as the default agent and as a
// human, through the real commands and an in-process daemon. Not parallel:
// it sets environment variables.
func TestMultiClientCLI(t *testing.T) {
	serveInProcess(t, isolate(t))

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out := run(t, 0, "start", "fake", "--program", prog, "--stop-on-entry", "--lease-policy", "handoff", "--bp", prog+":3", "--timeout", "20s")
	expectOutput(t, out, "stopped: entry")

	expectOutput(t, run(t, 0, "--as", "human:t", "bp", "add", prog+":5"), "2  human:t  ")
	expectOutput(t, run(t, exitState, "--as", "human:t", "continue"), "[LEASE_HELD]", "eyedbg lease grant human:t")
	expectOutput(t, run(t, 0, "--as", "human:t", "lease", "request", "--message", "let me"),
		"lease held by agent (policy handoff)", `requested by human:t: "let me"`)
	expectOutput(t, run(t, 0, "status"), `lease: agent (handoff); requested by human:t: "let me"; clients: agent, human:t`)
	expectOutput(t, run(t, 0, "lease", "grant", "human:t"), "lease held by human:t (policy handoff)")
	expectOutput(t, run(t, 0, "--as", "human:t", "continue", "--timeout", "20s"), "stopped: breakpoint", "lease: human:t (handoff); clients: agent, human:t")
	expectOutput(t, run(t, exitState, "bp", "rm", "2"), "[NOT_OWNER]", "belongs to human:t")
	expectOutput(t, run(t, 0, "bp", "rm", "all"), "removed 1 breakpoint(s); 1 of other clients kept (--force removes them)")
	expectOutput(t, run(t, 0, "bp", "ls", "--mine", "--as", "human:t"), "2  human:t  ")

	expectOutput(t, run(t, 0, "events", "--since", "0"),
		"started by agent: ", "(lease policy handoff)", "client human:t joined", "bp 2 added by human:t",
		"lease: agent gave it to human:t", "human:t: continue", "bp 1 removed by agent", "next: eyedbg events --since ")
	expectOutput(t, run(t, 0, "events", "--wait", "--timeout", "50ms", "--kind", "lease"), "(no new events within 50ms)")
	expectOutput(t, run(t, 0, "sessions"), "ID  ", "LEASE", "human:t")

	expectOutput(t, run(t, exitState, "stop"), "[LEASE_HELD]")
	expectOutput(t, run(t, 0, "--as", "human:t", "stop"), " ended")
	expectOutput(t, run(t, exitError, "--as", "robot", "status"), "[INVALID_REQUEST]", "set it with --as or EYEDBG_CLIENT")
}

// lapsDriver runs fake programs of 3 lines, 5 times over ("lap" is 1..5).
type lapsDriver struct{ fakeDriver }

func (lapsDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: daptest.ProgramArgs{Program: spec.Program, Lines: 3, Laps: 5, StopAtEntry: spec.StopOnEntry}.Map(),
	}, nil
}

// TestSharedConditionsCLI: the agent's and a human's breakpoints at one
// line with different conditions each stop the program when their own
// condition holds, and the output names whose breakpoint the stop is for.
// Not parallel: it sets environment variables.
func TestSharedConditionsCLI(t *testing.T) {
	serveWithDrivers(t, isolate(t), lapsDriver{})

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "start", "fake", "--program", prog, "--stop-on-entry", "--timeout", "20s"), "stopped: entry")
	expectOutput(t, run(t, 0, "bp", "add", prog+":2", "--if", "lap == 2"), "1  agent  ")
	expectOutput(t, run(t, 0, "--as", "human:t", "bp", "add", prog+":2", "--if", "lap == 4"), "2  human:t  ")

	expectOutput(t, run(t, 0, "continue", "--timeout", "20s"), "stopped: breakpoint", "\n  stopped for breakpoint 1 of agent\n")
	if out := run(t, 0, "eval", "lap"); !strings.HasPrefix(out, "2 ") {
		t.Errorf("eval lap = %q, want 2", out)
	}

	expectOutput(t, run(t, 0, "continue", "--timeout", "20s"), "\n  stopped for breakpoint 2 of human:t\n")
	if out := run(t, 0, "eval", "lap"); !strings.HasPrefix(out, "4 ") {
		t.Errorf("eval lap = %q, want 4", out)
	}

	var snap struct {
		Session struct {
			Stop struct {
				Breakpoints []api.StopBreakpoint `json:"breakpoints"`
			} `json:"stop"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(run(t, 0, "status", "--json")), &snap); err != nil {
		t.Fatal(err)
	}

	if want := []api.StopBreakpoint{{ID: 2, Owner: "human:t"}}; !slices.Equal(snap.Session.Stop.Breakpoints, want) {
		t.Errorf("status --json stop.breakpoints = %+v, want %+v", snap.Session.Stop.Breakpoints, want)
	}

	expectOutput(t, run(t, 0, "events", "--since", "0", "--kind", "stopped"), "; for breakpoint 1 of agent", "; for breakpoint 2 of human:t")
	expectOutput(t, run(t, 0, "stop"), " ended")
}

// TestSessionsWithoutDaemon lists and forgets a lost session from its
// files alone. Not parallel: it sets environment variables.
func TestSessionsWithoutDaemon(t *testing.T) {
	p := isolate(t)

	if err := os.MkdirAll(p.Sessions, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := daemon.NewFileStore(p.Sessions, 1).Save(api.SessionInfo{
		ID: "s-lost", Lang: "dotnet", Program: "/work/app.dll", State: api.StateRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "sessions"), "s-lost  dotnet  -        lost", "(lost: eyedbgd exited")
	expectOutput(t, run(t, exitState, "stop", "-s", "s-other"), "[NO_SESSION]")
	expectOutput(t, run(t, 0, "stop", "-s", "s-lost"), "session s-lost forgotten (it was lost)")

	if _, err := os.Stat(filepath.Join(p.Sessions, "s-lost.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("metadata after stop: %v, want it gone", err)
	}

	if out := run(t, 0, "sessions"); out != "" {
		t.Errorf("sessions after forgetting = %q, want nothing", out)
	}
}
