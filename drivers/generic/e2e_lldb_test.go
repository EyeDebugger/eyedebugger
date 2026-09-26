// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// End-to-end tests against the real lldb-dap (docs/CONVENTIONS.md §
// Testing): EYEDBG_E2E=1, cc/c++/rustc on PATH, and lldb-dap (EYEDBG_LLDB_DAP
// or PATH). Unlike python and dotnet there is no bundled download to fall
// back on: with EYEDBG_E2E=1, a missing compiler or lldb-dap fails the test
// (adapters.Find's own hint), it never skips silently.

// requireLldb skips t unless EYEDBG_E2E=1 (the whole suite is opt-in) and,
// when EYEDBG_E2E_LANGS is set, it names lang.
func requireLldb(t *testing.T, lang string) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs cc/c++/rustc and lldb-dap, or EYEDBG_LLDB_DAP)")
	}

	requireLang(t, lang)
}

// requireLinuxAttach skips unless this is Linux with ptrace_scope 0: native
// attach to a process that isn't a child needs it (generic.nativeAttachHint;
// macOS instead needs the task_for_pid entitlement, not checked here).
func requireLinuxAttach(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skip("native attach needs Linux (macOS needs the task_for_pid entitlement)")
	}

	if data, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope"); err == nil && strings.TrimSpace(string(data)) != "0" {
		t.Skip("kernel.yama.ptrace_scope is not 0 (sudo sysctl kernel.yama.ptrace_scope=0)")
	}
}

// lldbApp is a compiled native sample app (mirrors pyApp, e2e_test.go).
type lldbApp struct {
	dir, src, bin string
}

// line returns the line of app.src that ends in "// marker: NAME" (the
// c/cpp/rust sample apps' convention; pyApp.line is Python's "# marker:").
func (a lldbApp) line(t *testing.T, marker string) int {
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

// compileApp copies testdata/apps/<lang>/basic to a temporary directory and
// compiles srcName with compiler and flags into "app". A missing compiler
// fails the test (EYEDBG_E2E=1 never skips silently).
//
// The directory is resolved with filepath.EvalSymlinks (as fakeProgram in
// internal/session/fake_test.go and TestResolveSpecResolvesSymlinks in
// internal/session/symlink_test.go already do): on macOS t.TempDir() is
// under /var/folders, a symlink to /private/var, and the compiler would
// record that unresolved path in debug info while the session sends the
// resolved path when it sets breakpoints, so lldb-dap's full-path match
// would never bind.
func compileApp(t *testing.T, lang, srcName, compiler string, flags ...string) lldbApp {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", lang, "basic"))); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, srcName)
	bin := filepath.Join(dir, "app")
	args := append(slices.Clone(flags), "-o", bin, src)

	cmd := exec.CommandContext(t.Context(), compiler, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", compiler, strings.Join(args, " "), err, out)
	}

	return lldbApp{dir: dir, src: src, bin: bin}
}

func compileC(t *testing.T) lldbApp {
	t.Helper()

	return compileApp(t, "c", "main.c", "cc", "-g", "-O0")
}

func compileCpp(t *testing.T) lldbApp {
	t.Helper()

	return compileApp(t, "cpp", "main.cpp", "c++", "-g", "-O0")
}

func compileRust(t *testing.T) lldbApp {
	t.Helper()

	return compileApp(t, "rust", "main.rs", "rustc", "-g", "-C", "opt-level=0")
}

// lldbLoopCases: c, cpp and rust's sample apps behave the same way (see
// testdata/apps/README.md): items {1,2,3,4,5}, price(x) = x*10, total 150
// unconditionally.
var lldbLoopCases = []struct {
	lang    string
	compile func(t *testing.T) lldbApp
}{
	{"c", compileC},
	{"cpp", compileCpp},
	{"rust", compileRust},
}

// startNative starts app's compiled binary with mode as its argument, and
// waits for the first stop or the exit.
func startNative(t *testing.T, m *session.Manager, lang string, app lldbApp, mode string, p api.StartParams) (*session.Session, api.Snapshot) {
	t.Helper()

	p.Lang = lang
	p.Program = app.bin
	p.Args = []string{mode}

	if p.ClientDir == "" {
		p.ClientDir = app.dir
	}

	s, err := m.Start(t.Context(), agent, p)
	if err != nil {
		t.Fatalf("start %s %s: %v", lang, mode, err)
	}

	snap := s.Wait(t.Context(), 0, e2eWait, api.DumpSpec{Dump: []string{api.DumpLocals, api.DumpStack}})

	return s, snap
}

// TestLldbLoop: a conditional breakpoint, locals, eval, set, step, an
// eval refused as a side effect, and the exit — the lldb-dap analog of
// TestPythonLoop, over c, cpp and rust.
func TestLldbLoop(t *testing.T) {
	for _, tc := range lldbLoopCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireLldb(t, tc.lang)

			app := tc.compile(t)
			body := app.line(t, "loop-body")

			s, snap := startNative(t, pyManager(t), tc.lang, app, "loop", api.StartParams{
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

			snap = pyResume(t, s, session.ExecContinue)
			expectExit(t, s, snap, 0)

			if !strings.Contains(output(s), "total 190") {
				t.Fatalf("output = %q, want total 190 (100 + 40 + 50)", output(s))
			}
		})
	}
}

// TestLldbSharedConditions: per-owner conditions at a shared line on
// lldb-dap (sharedConditions), over c, cpp and rust.
func TestLldbSharedConditions(t *testing.T) {
	for _, tc := range lldbLoopCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireLldb(t, tc.lang)

			app := tc.compile(t)
			body := app.line(t, "loop-body")

			s, snap := startNative(t, pyManager(t), tc.lang, app, "loop", api.StartParams{
				Breakpoints: []api.BreakpointSpec{{File: app.src, Line: body, Condition: "i == 0"}},
			})
			sharedConditions(t, s, snap, app.src, body, 0)
		})
	}
}

// TestLldbEnvList: --env reaches the program through ${envList}, the shape
// lldb-dap <=19 needs.
func TestLldbEnvList(t *testing.T) {
	for _, tc := range lldbLoopCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireLldb(t, tc.lang)

			app := tc.compile(t)

			s, snap := startNative(t, pyManager(t), tc.lang, app, "loop", api.StartParams{
				LaunchSpec: api.LaunchSpec{Env: map[string]string{"EYEDBG_SAMPLE": "hello"}},
			})
			expectExit(t, s, snap, 0)

			if !strings.Contains(output(s), "env hello") {
				t.Fatalf("output = %q, want env hello (proves ${envList} on lldb-dap <=19)", output(s))
			}
		})
	}
}

// TestCppExceptions: --exceptions all maps to cpp_throw and stops at an
// uncaught (indeed, any) throw. lldb-dap's stop sits at frame 0 in
// __cxa_throw (libc++abi/libstdc++'s own throw entry point), not at the
// user's throw line, so this checks State/Reason/Exception (as TestGoPanic
// does for Delve's panic stop) and then requires some frame in snap.Stack at
// the throw line, rather than requiring frame 0 to be it.
func TestCppExceptions(t *testing.T) {
	requireLldb(t, "cpp")

	app := compileCpp(t)

	_, snap := startNative(t, pyManager(t), "cpp", app, "throw", api.StartParams{Exceptions: api.ExceptionsAll})

	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil || snap.Session.Stop.Reason != "exception" || snap.Exception == nil {
		t.Fatalf("throw stop = %+v (exception %+v), want stopped (exception)", snap.Session, snap.Exception)
	}

	line := app.line(t, "throw")
	if !slices.ContainsFunc(snap.Stack, func(f api.Frame) bool { return f.Line == line }) {
		t.Errorf("stack = %+v, want a frame at line %d (the throw)", snap.Stack, line)
	}
}

// pauseAttached pauses s, again (up to pauseAttempts times) while it keeps
// running. lldb-dap 18 answers a pause that arrives before its event thread
// has seen the program resume after configurationDone with success and no
// stop (request_pause ignores Process::Halt's "Process is not running."):
// a sub-millisecond window this in-process pause, sent the moment Start
// returns, hits often and a CLI round trip practically never.
func pauseAttached(t *testing.T, s *session.Session) api.Snapshot {
	t.Helper()

	for range pauseAttempts {
		snap, err := s.Resume(t.Context(), agent, session.ExecPause, 0, pauseWait, api.DumpSpec{})

		switch {
		case api.CodeOf(err) == api.CodeNotRunning: // the last pause's stop came after its wait
			return s.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
		case err != nil:
			t.Fatalf("pause: %v", err)
		case !snap.TimedOut:
			return snap
		}
	}

	t.Fatalf("still running after %d pauses", pauseAttempts)

	return api.Snapshot{}
}

// How TestCAttach pauses (pauseAttached).
const (
	pauseAttempts = 3
	pauseWait     = 5 * time.Second
)

// TestCAttach: attach, pause, detach, the process keeps running. Linux
// only (ptrace_scope 0): macOS needs the task_for_pid entitlement.
func TestCAttach(t *testing.T) {
	requireLldb(t, "c")
	requireLinuxAttach(t)

	app := compileC(t)

	cmd := exec.CommandContext(t.Context(), app.bin, "wait") //nolint:gosec // app.bin is what compileApp just built; "wait" is a fixed argument.

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

	s, err := pyManager(t).Start(t.Context(), agent, api.StartParams{Lang: "c", Attach: &api.AttachSpec{PID: pid}})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	if info := s.Info(); info.Mode != api.ModeAttach || info.PID != pid {
		t.Fatalf("info = %+v, want attached to %d", info, pid)
	}

	snap := pauseAttached(t, s)
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
