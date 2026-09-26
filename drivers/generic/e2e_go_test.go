// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// End-to-end tests against the real Delve (docs/CONVENTIONS.md § Testing):
// EYEDBG_E2E=1, go on PATH, and dlv (EYEDBG_DLV, PATH, or a managed install
// behind EYEDBG_E2E_NETWORK=1). Unlike lldb-dap's stdio, Delve's dlv dap
// only ever listens: these are the first real-adapter exercise of schema
// 1's connect transport (Step 7), which internal/cli's
// TestManifestLanguageConnectCLI otherwise only proves against the fake
// adapter. With EYEDBG_E2E=1 a missing go or dlv fails the test, it never
// skips silently.

// requireGoApp copies testdata/apps/go/basic to a temporary directory,
// after requireGo's checks (EYEDBG_E2E=1, EYEDBG_E2E_LANGS).
func requireGoApp(t *testing.T) goApp {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs go and dlv, or EYEDBG_DLV, or 'eyedbg adapters install delve')")
	}

	requireLang(t, "go")

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "go", "basic"))); err != nil {
		t.Fatal(err)
	}

	return goApp{dir: dir, src: filepath.Join(dir, "main.go")}
}

// goApp is the sample app copied for one test (mirrors pyApp, lldbApp): its
// own module, debugged from dir (debug mode) or a binary built into it
// (exec mode).
type goApp struct {
	dir, src string
}

// line returns the line of app.src that ends in "// marker: NAME".
func (a goApp) line(t *testing.T, marker string) int {
	t.Helper()

	f, err := os.Open(a.src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "// marker: "+marker) {
			return n
		}
	}

	t.Fatalf("no marker %q in %s", marker, a.src)

	return 0
}

// goManager is a manager with the bundled delve driver.
func goManager(t *testing.T) *session.Manager {
	t.Helper()

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{"dotnet"}})

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{Drivers: Drivers(reg), Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// buildGoApp builds app's package directory into "app" (an already-built
// binary, for exec mode or attach), with extra go build flags. cmd.Dir is
// app.dir (its own module, go.mod): "go build" on an absolute path
// belonging to a different module than the working directory's is refused
// ("outside main module or its selected dependencies").
func buildGoApp(t *testing.T, app goApp, flags ...string) string {
	t.Helper()

	bin := filepath.Join(app.dir, "app")
	args := append(append([]string{"build"}, flags...), "-o", bin, ".")

	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = app.dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s (in %s): %v\n%s", strings.Join(args, " "), app.dir, err, out)
	}

	return bin
}

// startGo starts app's package directory (debug mode; p.Program overrides
// it, e.g. exec mode's built binary) with mode as its argument, and waits
// for the first stop or the exit.
func startGo(t *testing.T, m *session.Manager, app goApp, mode string, p api.StartParams) (*session.Session, api.Snapshot) {
	t.Helper()

	p.Lang = "go"

	if p.Program == "" {
		p.Program = app.dir
	}

	if p.ClientDir == "" {
		p.ClientDir = app.dir
	}

	p.Args = []string{mode}

	s, err := m.Start(t.Context(), agent, p)
	if err != nil {
		t.Fatalf("start go %s: %v", mode, err)
	}

	snap := s.Wait(t.Context(), 0, e2eWait, api.DumpSpec{Dump: []string{api.DumpLocals, api.DumpStack}})

	return s, snap
}

// TestGoLoop: a conditional breakpoint, locals, eval, set, step, an eval
// refused as a side effect (a plain assignment, then a bare function call
// even with Delve's "call " evaluate prefix: the guard only looks at
// "NAME(", not what precedes it), a safe len() call allowed, and the exit
// — the Delve analog of TestLldbLoop, over schema 1's connect transport.
func TestGoLoop(t *testing.T) {
	app := requireGoApp(t)
	body := app.line(t, "loop-body")

	s, snap := startGo(t, goManager(t), app, "loop", api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: app.src, Line: body, Condition: "i == 3"}},
	})
	expectStop(t, snap, "breakpoint", body)

	if snap.Locals == nil || !slices.Contains(names(snap.Locals.Vars), "i") {
		t.Fatalf("stop locals = %+v, want i", snap.Locals)
	}

	if got := e2eEval(t, s, "i"); got != "3" {
		t.Fatalf("i = %s, want 3", got)
	}

	if res, err := s.Set(t.Context(), agent, api.SetParams{Variable: "total", Value: "100"}); err != nil || res.Value != "100" {
		t.Fatalf("set total 100 = %+v, %v", res, err)
	}

	snap = pyResume(t, s, session.ExecNext)
	expectStop(t, snap, "step", app.line(t, "append"))

	if _, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: "total = 5"}); api.CodeOf(err) != api.CodeSideEffects {
		t.Errorf("eval total = 5 = %v, want SIDE_EFFECTS", err)
	}

	if _, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: "call price(1)"}); api.CodeOf(err) != api.CodeSideEffects {
		t.Errorf("eval call price(1) = %v, want SIDE_EFFECTS", err)
	}

	if got := e2eEval(t, s, "len(items)"); got != "5" {
		t.Errorf("len(items) = %s, want 5", got)
	}

	snap = pyResume(t, s, session.ExecContinue)
	expectExit(t, s, snap, 0)

	if !strings.Contains(output(s), "total 190") {
		t.Fatalf("output = %q, want total 190 (100 + 40 + 50)", output(s))
	}
}

// TestGoSharedConditions: per-owner conditions at a shared line on Delve
// (sharedConditions).
func TestGoSharedConditions(t *testing.T) {
	app := requireGoApp(t)
	body := app.line(t, "loop-body")

	s, snap := startGo(t, goManager(t), app, "loop", api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: app.src, Line: body, Condition: "i == 0"}},
	})
	sharedConditions(t, s, snap, app.src, body, 0)
}

// TestGoEntryAndExec: --stop-on-entry, and --opt mode=exec on a binary
// built with -gcflags=all=-N -l (optimizations and inlining off, so
// breakpoints and locals are reliable, mirroring 'dlv debug's own default).
// service/dap/server.go's onConfigurationDoneRequest sends the "entry"
// stopped event before the debuggee has run any code (the process is
// halted at its OS entry point, before Go's runtime has initialized), so
// unlike the other languages' entry stop this doesn't land on a line in
// main.go: only the state and reason are checked here.
func TestGoEntryAndExec(t *testing.T) {
	app := requireGoApp(t)
	m := goManager(t)

	s, snap := startGo(t, m, app, "loop", api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil || snap.Session.Stop.Reason != "entry" {
		t.Fatalf("stop-on-entry = %+v, want stopped (entry)", snap.Session)
	}

	t.Logf("entry stop frame: %+v", snap.Frame)

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)

	bin := buildGoApp(t, app, "-gcflags=all=-N -l")

	s, snap = startGo(t, m, app, "loop", api.StartParams{
		LaunchSpec: api.LaunchSpec{Program: bin, Options: map[string]string{"mode": "exec"}},
	})
	expectExit(t, s, snap, 0)

	if !strings.Contains(output(s), "total 150") {
		t.Fatalf("exec run output = %q", output(s))
	}
}

// TestGoPanic: --exceptions uncaught stops at the unrecovered panic (the
// panic() call appears somewhere in the reported stack, though Delve's
// unrecovered-panic breakpoint sits inside the runtime, not necessarily at
// frame 0), and continuing exits 2 (Go's exit code for an unhandled
// panic).
func TestGoPanic(t *testing.T) {
	app := requireGoApp(t)

	s, snap := startGo(t, goManager(t), app, "panic", api.StartParams{Exceptions: api.ExceptionsUncaught})

	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil || snap.Session.Stop.Reason != "exception" || snap.Exception == nil {
		t.Fatalf("panic stop = %+v (exception %+v), want stopped (exception)", snap.Session, snap.Exception)
	}

	line := app.line(t, "panic")
	if !slices.ContainsFunc(snap.Stack, func(f api.Frame) bool { return f.Line == line }) {
		t.Errorf("stack = %+v, want a frame at line %d (the panic call)", snap.Stack, line)
	}

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 2)
}

// TestGoFunctionBreakpoint: func:main.price (Delve's function names are
// package-qualified, unlike Python's bare names) stops on price's first
// call; removing it (price runs 5 times over items) lets the program run
// to exit, as TestPythonFunctionBreakpointAndRunUntil does.
func TestGoFunctionBreakpoint(t *testing.T) {
	app := requireGoApp(t)

	s, snap := startGo(t, goManager(t), app, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{Function: "main.price"}}})
	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Name != "main.price" {
		t.Fatalf("func:main.price = %+v (frame %+v), want stopped in main.price", snap.Session, snap.Frame)
	}

	if _, _, err := s.RemoveBreakpoint(t.Context(), agent, 0, false); err != nil {
		t.Fatal(err)
	}

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)
}

// TestGoAttach: attach, pause, detach, the process keeps running. Linux
// only (ptrace_scope 0): macOS needs the task_for_pid entitlement.
func TestGoAttach(t *testing.T) {
	app := requireGoApp(t)
	requireLinuxAttach(t)

	bin := buildGoApp(t, app)

	cmd := exec.CommandContext(t.Context(), bin, "wait")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	lines := bufio.NewScanner(out)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "pid ") {
		t.Fatalf("program said %q, want pid PID", lines.Text())
	}

	pid, err := strconv.Atoi(strings.TrimPrefix(lines.Text(), "pid "))
	if err != nil {
		t.Fatal(err)
	}

	s, err := goManager(t).Start(t.Context(), agent, api.StartParams{Lang: "go", Attach: &api.AttachSpec{PID: pid}})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	if info := s.Info(); info.Mode != api.ModeAttach || info.PID != pid {
		t.Fatalf("info = %+v, want attached to %d", info, pid)
	}

	snap := pyResume(t, s, session.ExecPause)
	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil || snap.Session.Stop.Reason != "pause" {
		t.Fatalf("pause = %+v, want stopped (pause)", snap.Session)
	}

	if err := s.Detach(t.Context(), agent); err != nil {
		t.Fatal(err)
	}

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process should still be running after detach: %v", err)
	}
}

// TestGoManagedInstall downloads Delve with 'adapters install' into a
// temporary data directory and debugs with that copy. Needs
// EYEDBG_E2E_NETWORK=1 (github.com). Not parallel: it sets the
// environment.
func TestGoManagedInstall(t *testing.T) {
	app := requireGoApp(t)

	if os.Getenv(envE2ENetwork) != "1" {
		t.Skip("set " + envE2ENetwork + "=1 to download delve")
	}

	t.Setenv(adapters.EnvDataDir, t.TempDir())
	t.Setenv("EYEDBG_DLV", "")

	m := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled()}).Adapter("delve")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	if _, err := adapters.Install(ctx, http.DefaultClient, m); err != nil {
		t.Fatalf("install delve: %v", err)
	}

	loc, err := adapters.Find(m)
	if err != nil || loc.Source != adapters.FoundInstalled {
		t.Fatalf("find = %+v, %v; want the installed copy", loc, err)
	}

	s, snap := startGo(t, goManager(t), app, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{File: app.src, Line: app.line(t, "print")}}})
	expectStop(t, snap, "breakpoint", app.line(t, "print"))
	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)
}
