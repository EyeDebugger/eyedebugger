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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// End-to-end tests against the real debugpy (docs/CONVENTIONS.md § Testing):
// EYEDBG_E2E=1, a Python 3.10+ with debugpy (EYEDBG_PYTHON, a venv, or
// PATH), or EYEDBG_DATA_DIR holding 'eyedbg adapters install debugpy'.
const (
	envE2E        = "EYEDBG_E2E"
	envE2ENetwork = "EYEDBG_E2E_NETWORK"
	// envE2ELangs narrows which languages the end-to-end tests exercise:
	// unset runs every language; set (comma-separated) skips a language not
	// listed, naming the variable (never silently: docs/CONVENTIONS.md §
	// Testing) — duplicated per package, like markerLine.
	envE2ELangs = "EYEDBG_E2E_LANGS"
	// e2eWait bounds every wait for the real program.
	e2eWait = 60 * time.Second
)

// requireLang skips t unless EYEDBG_E2E_LANGS is unset or names lang.
func requireLang(t *testing.T, lang string) {
	t.Helper()

	list := os.Getenv(envE2ELangs)
	if list == "" {
		return
	}

	if !slices.Contains(strings.Split(list, ","), lang) {
		t.Skipf("%s=%s excludes %s", envE2ELangs, list, lang)
	}
}

// appsDir holds the Python sample apps.
var appsDir = filepath.Join("..", "..", "testdata", "apps", "python")

// pyApp is the sample app copied for one test.
type pyApp struct {
	dir, src string
}

// requirePython skips the test unless the end-to-end tests are enabled,
// and copies the sample app to a temporary directory.
func requirePython(t *testing.T) pyApp {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs Python 3.10+ with debugpy, or 'eyedbg adapters install debugpy')")
	}

	requireLang(t, "python")

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(appsDir, "basic"))); err != nil {
		t.Fatal(err)
	}

	_ = os.RemoveAll(filepath.Join(dir, "__pycache__"))

	return pyApp{dir: dir, src: filepath.Join(dir, "app.py")}
}

// line returns the line of the app that ends in "# marker: NAME".
func (a pyApp) line(t *testing.T, marker string) int {
	t.Helper()

	f, err := os.Open(a.src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "# marker: "+marker) {
			return n
		}
	}

	t.Fatalf("no marker %q in %s", marker, a.src)

	return 0
}

// pyManager is a manager with the bundled python driver.
func pyManager(t *testing.T) *session.Manager {
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

// startPy starts app with mode (and p's other fields) and waits for the
// first stop or the exit.
func startPy(t *testing.T, m *session.Manager, a pyApp, mode string, p api.StartParams) (*session.Session, api.Snapshot) {
	t.Helper()

	p.Lang = "python"
	if p.Program == "" && p.Options["module"] == "" {
		p.Program = a.src
	}

	if p.ClientDir == "" {
		p.ClientDir = a.dir
	}

	p.Args = []string{mode}

	s, err := m.Start(t.Context(), agent, p)
	if err != nil {
		t.Fatalf("start python %s: %v", mode, err)
	}

	snap := s.Wait(t.Context(), 0, e2eWait, api.DumpSpec{Dump: []string{api.DumpLocals, api.DumpStack}})

	return s, snap
}

func pyResume(t *testing.T, s *session.Session, kind string) api.Snapshot {
	t.Helper()

	snap, err := s.Resume(t.Context(), agent, kind, 0, e2eWait, api.DumpSpec{Dump: []string{api.DumpChanged}})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}

	return snap
}

// output is everything the program printed (stdout and stderr).
func output(s *session.Session) string {
	var b strings.Builder

	for _, l := range s.Output(0, 0).Lines {
		if l.Category == "stdout" || l.Category == "stderr" {
			b.WriteString(l.Text)
		}
	}

	return b.String()
}

func expectExit(t *testing.T, s *session.Session, snap api.Snapshot, code int) {
	t.Helper()

	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != code {
		t.Fatalf("session = %+v, want exited with %d; output:\n%s", snap.Session, code, output(s))
	}
}

func names(vars []api.Var) []string {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Name)
	}

	return out
}

// TestPythonLoop: a conditional breakpoint, locals, scopes, stack, step,
// set, eval with and without side effects, and the exit.
func TestPythonLoop(t *testing.T) {
	a := requirePython(t)
	body := a.line(t, "loop-body")

	s, snap := startPy(t, pyManager(t), a, "loop", api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: a.src, Line: body, Condition: "i == 3"}},
	})
	expectStop(t, snap, "breakpoint", body)

	if snap.Frame.Name != "compute" || snap.Locals == nil || !slices.Contains(names(snap.Locals.Vars), "i") ||
		slices.Contains(names(snap.Locals.Vars), "sys") {
		t.Fatalf("stop in %q with locals %v; want compute's locals only (i, not the module's sys)", snap.Frame.Name, snap.Locals)
	}

	if got := e2eEval(t, s, "i"); got != "3" {
		t.Fatalf("i = %s, want 3", got)
	}

	checkScopesAndStack(t, s, snap)

	if res, err := s.Set(t.Context(), agent, api.SetParams{Variable: "total", Value: "100"}); err != nil || res.Value != "100" {
		t.Fatalf("set total 100 = %+v, %v", res, err)
	}

	snap = pyResume(t, s, session.ExecNext)
	expectStop(t, snap, "step", a.line(t, "append"))

	if snap.Changes == nil || !slices.Contains(names(snap.Changes.Vars), "total") {
		t.Fatalf("changes after next = %+v, want total", snap.Changes)
	}

	checkPythonEval(t, s)

	snap = pyResume(t, s, session.ExecContinue)
	expectExit(t, s, snap, 0)

	if !strings.Contains(output(s), "total 190") {
		t.Fatalf("output = %q, want total 190 (100 + 40 + 50)", output(s))
	}
}

func checkScopesAndStack(t *testing.T, s *session.Session, snap api.Snapshot) {
	t.Helper()

	scopes, err := s.Vars(t.Context(), 0, 1, 0)
	if err != nil || len(scopes) != 2 || scopes[0].Name != "Locals" || scopes[1].Name != "Globals" {
		t.Fatalf("vars = %+v, %v; want Locals and Globals", scopes, err)
	}

	if g := names(scopes[1].Vars); slices.Contains(g, "__name__") || slices.Contains(g, "price") || !slices.Contains(g, "sys") {
		t.Errorf("Globals = %v; want modules, but no dunders or functions", g)
	}

	var frames []string
	for _, f := range snap.Stack {
		frames = append(frames, f.Name)
	}

	if len(frames) < 3 || frames[0] != "compute" || frames[1] != "main" || frames[2] != "<module>" {
		t.Errorf("stack = %v, want compute, main, <module>", frames)
	}
}

func checkPythonEval(t *testing.T, s *session.Session) {
	t.Helper()

	if got := e2eEval(t, s, "total"); got != "140" {
		t.Errorf("total = %s, want 140", got)
	}

	if got := e2eEval(t, s, "len(items)"); got != "5" {
		t.Errorf("len(items) = %s, want 5", got)
	}

	_, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: "items.append(9)"})
	if api.CodeOf(err) != api.CodeSideEffects {
		t.Errorf("eval items.append(9) = %v, want SIDE_EFFECTS", err)
	}

	if _, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: "items.append(9)", AllowSideEffects: true}); err != nil {
		t.Errorf("eval items.append(9) --allow-side-effects: %v", err)
	}

	if got := e2eEval(t, s, "len(items)"); got != "6" {
		t.Errorf("len(items) after append = %s, want 6", got)
	}

	if _, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: "nosuch"}); api.CodeOf(err) != api.CodeAdapterFailed {
		t.Errorf("eval nosuch = %v, want ADAPTER_ERROR", err)
	}
}

func e2eEval(t *testing.T, s *session.Session, expr string) string {
	t.Helper()

	res, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: expr})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}

	return res.Value
}

// TestPythonEntryAndModule: stop on entry, and a module run with its
// working directory the app's.
func TestPythonEntryAndModule(t *testing.T) {
	a := requirePython(t)
	m := pyManager(t)

	s, snap := startPy(t, m, a, "loop", api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	expectStop(t, snap, "entry", a.line(t, "entry"))

	if snap.Frame.Name != "<module>" {
		t.Fatalf("entry stop in %q, want <module>", snap.Frame.Name)
	}
	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)

	s, snap = startPy(t, m, a, "loop", api.StartParams{LaunchSpec: api.LaunchSpec{Options: map[string]string{"module": "app"}}})
	expectExit(t, s, snap, 0)

	if !strings.Contains(output(s), "total 150") {
		t.Fatalf("module run output = %q", output(s))
	}
}

// TestPythonHitsAndLogpoints: M5's emulated --hit and --log on debugpy.
func TestPythonHitsAndLogpoints(t *testing.T) {
	a := requirePython(t)
	m := pyManager(t)
	body := a.line(t, "loop-body")

	s, snap := startPy(t, m, a, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{File: a.src, Line: body, HitCondition: "3"}}})
	expectStop(t, snap, "breakpoint", body)

	if got := e2eEval(t, s, "i"); got != "2" {
		t.Errorf("--hit 3 stopped at i = %s, want 2", got)
	}

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)

	s, snap = startPy(t, m, a, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{File: a.src, Line: a.line(t, "append"), LogMessage: "i={i}"}}})
	expectExit(t, s, snap, 0)

	var logs []string

	for _, l := range s.Output(0, 0).Lines {
		if l.Category == "logpoint" {
			logs = append(logs, strings.TrimSpace(l.Text))
		}
	}

	if want := []string{"i=0", "i=1", "i=2", "i=3", "i=4"}; !slices.Equal(logs, want) {
		t.Errorf("logpoint output = %q, want %q", logs, want)
	}
}

// TestPythonSharedConditions: per-owner conditions at a shared line on
// debugpy (sharedConditions).
func TestPythonSharedConditions(t *testing.T) {
	a := requirePython(t)
	body := a.line(t, "loop-body")

	s, snap := startPy(t, pyManager(t), a, "loop", api.StartParams{
		Breakpoints: []api.BreakpointSpec{{File: a.src, Line: body, Condition: "i == 0"}},
	})
	sharedConditions(t, s, snap, a.src, body, 0)
}

// sharedConditions is S1's scenario at loop-body line body of file, whose
// loop index i runs first..first+4, from snap: the agent's breakpoint
// there with i == first just stopped. The agent changes its condition to
// i == first+1 and a human adds one with i == first+3 at the same line:
// each stops once, when its own condition holds (eyedbg evaluates both,
// through the adapter's evaluate), and the stop names whose breakpoint it
// is for. Another client's logpoint added there logs every later pass and
// suppresses neither stop.
func sharedConditions(t *testing.T, s *session.Session, snap api.Snapshot, file string, body, first int) {
	t.Helper()

	var (
		human  = api.Client{ID: "human:e2e", Kind: api.KindHuman, Name: "e2e"}
		logger = api.Client{ID: "agent:log", Kind: api.KindAgent}
		cond   = func(n int) string { return "i == " + strconv.Itoa(n) }
	)

	expectStop(t, snap, "breakpoint", body)
	expectI(t, s, first)

	add := func(c api.Client, spec api.BreakpointSpec) api.Breakpoint {
		t.Helper()

		spec.File, spec.Line = file, body

		b, err := s.AddBreakpoint(t.Context(), c, spec)
		if err != nil {
			t.Fatalf("%s: bp add %+v: %v", c.ID, spec, err)
		}

		return b
	}

	before := s.Breakpoints(agent.ID)

	mine := add(agent, api.BreakpointSpec{Condition: cond(first + 1)})
	if len(before) != 1 || mine.ID != before[0].ID {
		t.Fatalf("re-added breakpoint %d, want the agent's one of %+v", mine.ID, before)
	}

	theirs := add(human, api.BreakpointSpec{Condition: cond(first + 3)})

	snap = pyResume(t, s, session.ExecContinue)
	expectStop(t, snap, "breakpoint", body)
	expectI(t, s, first+1)
	expectFor(t, snap, api.StopBreakpoint{ID: mine.ID, Owner: agent.ID})

	add(logger, api.BreakpointSpec{LogMessage: "i={i}"})

	snap = pyResume(t, s, session.ExecContinue)
	expectStop(t, snap, "breakpoint", body)
	expectI(t, s, first+3)
	expectFor(t, snap, api.StopBreakpoint{ID: theirs.ID, Owner: human.ID})

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)

	var logs []string

	for _, l := range s.Output(0, 0).Lines {
		if l.Category == "logpoint" {
			logs = append(logs, strings.TrimSpace(l.Text))
		}
	}

	if want := []string{"i=" + strconv.Itoa(first+2), "i=" + strconv.Itoa(first+3), "i=" + strconv.Itoa(first+4)}; !slices.Equal(logs, want) {
		t.Errorf("logpoint output = %q, want %q", logs, want)
	}
}

// expectI checks the loop index i at the stop.
func expectI(t *testing.T, s *session.Session, want int) {
	t.Helper()

	if got := e2eEval(t, s, "i"); got != strconv.Itoa(want) {
		t.Fatalf("stopped at i = %s, want %d", got, want)
	}
}

// expectFor checks the breakpoints the stop is for.
func expectFor(t *testing.T, snap api.Snapshot, want ...api.StopBreakpoint) {
	t.Helper()

	if got := snap.Session.Stop.Breakpoints; !slices.Equal(got, want) {
		t.Errorf("stop for %+v, want %+v", got, want)
	}
}

// TestPythonFunctionBreakpointAndRunUntil: func:price stops in price, and
// run-until reaches a text anchor.
func TestPythonFunctionBreakpointAndRunUntil(t *testing.T) {
	a := requirePython(t)

	s, snap := startPy(t, pyManager(t), a, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{Function: "price"}}})
	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Name != "price" {
		t.Fatalf("func:price = %+v (stop %+v, frame %+v), want stopped in price", snap.Session, snap.Session.Stop, snap.Frame)
	}

	t.Logf("function breakpoint stop reason %q", snap.Session.Stop.Reason)

	if _, _, err := s.RemoveBreakpoint(t.Context(), agent, 0, false); err != nil {
		t.Fatal(err)
	}

	snap, err := s.RunUntil(t.Context(), agent, api.BreakpointSpec{File: a.src, Anchor: `print("total", total)`}, 0, e2eWait, api.DumpSpec{})
	if err != nil || snap.Reached == nil || !*snap.Reached || snap.Frame == nil || snap.Frame.Line != a.line(t, "print") {
		t.Fatalf("run-until = %+v (frame %+v), %v; want reached at the print", snap.Session, snap.Frame, err)
	}

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)
}

// TestPythonExceptions: the exception modes map to debugpy's filters.
func TestPythonExceptions(t *testing.T) {
	a := requirePython(t)
	m := pyManager(t)

	s, snap := startPy(t, m, a, "raise", api.StartParams{Exceptions: api.ExceptionsAll})
	expectStop(t, snap, "exception", a.line(t, "caught"))

	if snap.Exception == nil || !strings.Contains(snap.Exception.Type+snap.Exception.ID, "ValueError") {
		t.Errorf("exception = %+v, want ValueError", snap.Exception)
	}

	stops := 1
	for snap = pyResume(t, s, session.ExecContinue); snap.Session.State == api.StateStopped && stops < 10; stops++ {
		snap = pyResume(t, s, session.ExecContinue)
	}

	expectExit(t, s, snap, 1)
	t.Logf("all: %d stops", stops)

	s, snap = startPy(t, m, a, "raise", api.StartParams{Exceptions: api.ExceptionsUncaught})
	expectStop(t, snap, "exception", a.line(t, "fail"))

	if snap.Exception == nil || !strings.Contains(snap.Exception.Type+snap.Exception.ID, "RuntimeError") {
		t.Errorf("exception = %+v, want RuntimeError", snap.Exception)
	}

	expectExit(t, s, pyResume(t, s, session.ExecContinue), 1)

	s, snap = startPy(t, m, a, "raise", api.StartParams{})
	expectExit(t, s, snap, 1)

	if !strings.Contains(output(s), "RuntimeError: uncaught") {
		t.Errorf("output = %q, want the traceback", output(s))
	}
}

// TestPythonChildAndRefusals: a child process runs (undebugged); attach
// and a missing interpreter are refused.
func TestPythonChildAndRefusals(t *testing.T) {
	a := requirePython(t)
	m := pyManager(t)

	s, snap := startPy(t, m, a, "child", api.StartParams{})
	expectExit(t, s, snap, 0)

	if !strings.Contains(output(s), "child ok") {
		t.Fatalf("output = %q, want child ok", output(s))
	}

	_, err := m.Start(t.Context(), agent, api.StartParams{Lang: "python", Attach: &api.AttachSpec{PID: os.Getpid()}})
	if api.CodeOf(err) != api.CodeUnsupported {
		t.Errorf("attach python = %v, want UNSUPPORTED_BY_ADAPTER", err)
	}

	_, err = m.Start(t.Context(), agent, api.StartParams{
		Lang: "python", LaunchSpec: api.LaunchSpec{Program: a.src, Options: map[string]string{"python": filepath.Join(a.dir, "nonexistent")}},
	})
	if api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("--opt python=nonexistent = %v, want INVALID_REQUEST", err)
	}
}

// TestPythonManagedInstall downloads debugpy with 'adapters install' into
// a temporary data directory and debugs with that copy. Needs
// EYEDBG_E2E_NETWORK=1 (files.pythonhosted.org). Not parallel: it sets
// the environment.
func TestPythonManagedInstall(t *testing.T) {
	a := requirePython(t)

	if os.Getenv(envE2ENetwork) != "1" {
		t.Skip("set " + envE2ENetwork + "=1 to download debugpy")
	}

	t.Setenv(adapters.EnvDataDir, t.TempDir())
	t.Setenv("EYEDBG_PYTHON", "")

	m := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled()}).Adapter("debugpy")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	if _, err := adapters.Install(ctx, http.DefaultClient, m); err != nil {
		t.Fatalf("install debugpy: %v", err)
	}

	rt, err := adapters.ResolvePython(t.Context(), m, adapters.PythonInput{Cwd: a.dir})
	if err != nil || rt.RootSource != adapters.RootInstalled {
		t.Fatalf("resolve = %+v, %v; want the installed copy", rt, err)
	}

	s, snap := startPy(t, pyManager(t), a, "loop", api.StartParams{Breakpoints: []api.BreakpointSpec{{File: a.src, Line: a.line(t, "print")}}})
	expectStop(t, snap, "breakpoint", a.line(t, "print"))
	expectExit(t, s, pyResume(t, s, session.ExecContinue), 0)
}
