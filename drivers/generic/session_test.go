// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// testWait bounds every wait; none should come close.
const testWait = 20 * time.Second

var agent = api.Client{ID: "agent", Kind: api.KindAgent}

// fakeManifest is a user manifest for the language "fakelang", served by
// the daptest fake adapter (this test binary): a language that exists
// only as data. attached is the program an attach attaches to; without
// attach the manifest has no attach template.
func fakeManifest(t *testing.T, attached string) map[string]any {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	m := map[string]any{
		"schema": 1, "name": "fakedbg", "description": "The fake test adapter", "version": "1.0",
		"adapter": map[string]any{
			"id": "fake", "entry": exe, "args": []any{"-test.run=^$"},
			"environment": map[string]any{daptest.EnvFakeAdapter: "1"},
		},
		"language": map[string]any{"name": "fakelang", "extensions": []any{".fake"}},
		"options": map[string]any{
			"lines": map[string]any{"type": "int", "default": "10", "help": "the program's length"},
			"laps":  map[string]any{"type": "int", "help": "how often it runs"},
		},
		"launch": map[string]any{
			"require": []any{"program"},
			"arguments": map[string]any{
				"program": "${program}", "lines": "${opt.lines}", "stopAtEntry": "${stopOnEntry}", "laps": "${opt.laps}",
			},
		},
		"attachUnsupported": "start it with 'eyedbg start fakelang'",
		"exceptions":        map[string]any{"all": []any{"all"}, "uncaught": []any{"user-unhandled"}},
		"evalGuard":         map[string]any{"nonCallWords": []any{"not"}, "safeCalls": []any{"len"}, "assignOps": []any{"="}},
	}

	if attached != "" {
		m["attach"] = map[string]any{"arguments": map[string]any{
			"program": attached, "lines": 10, "hang": true, "processId": "${pid}",
		}}
	}

	return m
}

// privateDir is a new temporary directory only the user can write: under
// umask 002 t.TempDir's own directories are group-writable, which the
// manifest permission check refuses.
func privateDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A directory needs its x bit; 0700 is private.
		t.Fatal(err)
	}

	return dir
}

// userRegistry writes manifest into a new user manifest directory and
// loads it (and nothing else).
func userRegistry(t *testing.T, manifest map[string]any) *adapters.Registry {
	t.Helper()

	raw, err := json.Marshal(manifest) // escapes Windows paths
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(privateDir(t), "adapters")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "fake.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	reg := adapters.Load(adapters.LoadConfig{UserDir: dir})
	if p := reg.Problems(); len(p) > 0 {
		t.Fatalf("manifest problems: %+v", p)
	}

	return reg
}

// manager runs sessions with the generic drivers of reg.
func manager(t *testing.T, reg *adapters.Registry) *session.Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{Drivers: Drivers(reg), Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// fakeProgram writes a program file of n lines.
func fakeProgram(t *testing.T, n int) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "prog.fake")
	if err := os.WriteFile(p, []byte(strings.Repeat("line\n", n)), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func startFake(t *testing.T, m *session.Manager, p api.StartParams) *session.Session {
	t.Helper()

	p.Lang = "fakelang"

	s, err := m.Start(t.Context(), agent, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	return s
}

func expectStop(t *testing.T, snap api.Snapshot, reason string, line int) {
	t.Helper()

	st := snap.Session
	if st.State != api.StateStopped || st.Stop == nil || st.Stop.Reason != reason || snap.Frame == nil || snap.Frame.Line != line {
		t.Fatalf("snapshot = %+v (frame %+v), want stopped (%s) at line %d", st, snap.Frame, reason, line)
	}
}

func eval(t *testing.T, s *session.Session, expr string) string {
	t.Helper()

	res, err := s.Eval(t.Context(), agent, api.EvalParams{Expression: expr})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}

	return res.Value
}

// TestManifestLanguage debugs a program of a language that exists only
// as a manifest written by the test: nothing in Go knows "fakelang".
func TestManifestLanguage(t *testing.T) {
	t.Parallel()

	m := manager(t, userRegistry(t, fakeManifest(t, "")))
	prog := fakeProgram(t, 8)

	s := startFake(t, m, api.StartParams{
		LaunchSpec:  api.LaunchSpec{Program: prog, Options: map[string]string{"lines": "8"}},
		Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
	})
	expectStop(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "breakpoint", 3)

	if _, err := s.AddBreakpoint(t.Context(), agent, api.BreakpointSpec{File: prog, Line: 5}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.Resume(t.Context(), agent, session.ExecContinue, 0, testWait, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, snap, "breakpoint", 5)

	if got := eval(t, s, "line"); got != "5" {
		t.Fatalf("eval line = %q", got)
	}

	if _, err := s.Set(t.Context(), agent, api.SetParams{Variable: "x", Value: "42"}); err != nil {
		t.Fatal(err)
	}

	if got := eval(t, s, "x"); got != "42" {
		t.Fatalf("x after set = %q", got)
	}

	if _, err := s.Exceptions(t.Context(), agent, api.ExceptionsParams{Mode: api.ExceptionsAll}); err != nil {
		t.Fatal(err)
	}

	if got := eval(t, s, "$filters"); got != "all" {
		t.Fatalf("exception filters for all = %q, want the manifest's [all]", got)
	}

	_, err = s.Eval(t.Context(), agent, api.EvalParams{Expression: "f(1)"})
	if api.CodeOf(err) != api.CodeSideEffects || !strings.Contains(err.Error(), "call a function (f)") {
		t.Fatalf("eval f(1) = %v, want SIDE_EFFECTS from the manifest's guard", err)
	}

	snap, err = s.Resume(t.Context(), agent, session.ExecContinue, 0, testWait, api.DumpSpec{})
	if err != nil || snap.Session.State != api.StateExited {
		t.Fatalf("continue to the end = %+v, %v; want exited", snap.Session, err)
	}
}

// TestManifestOptions: a typed option reaches the launch arguments (laps),
// and M5's emulated hit counts and logpoints work through the manifest.
func TestManifestOptions(t *testing.T) {
	t.Parallel()

	m := manager(t, userRegistry(t, fakeManifest(t, "")))
	prog := fakeProgram(t, 3)

	s := startFake(t, m, api.StartParams{
		LaunchSpec: api.LaunchSpec{Program: prog, StopOnEntry: true, Options: map[string]string{"lines": "3", "laps": "5"}},
	})
	expectStop(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)

	for _, spec := range []api.BreakpointSpec{{File: prog, Line: 2, HitCondition: "4"}, {File: prog, Line: 3, LogMessage: "lap {lap}"}} {
		if _, err := s.AddBreakpoint(t.Context(), agent, spec); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := s.Resume(t.Context(), agent, session.ExecContinue, 0, testWait, api.DumpSpec{})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, snap, "breakpoint", 2)

	if got := eval(t, s, "lap"); got != "4" {
		t.Fatalf("--hit 4 stopped in lap %s", got)
	}

	if snap, err = s.Resume(t.Context(), agent, session.ExecContinue, 0, testWait, api.DumpSpec{}); err != nil || snap.Session.State != api.StateExited {
		t.Fatalf("continue = %+v, %v; want exited", snap.Session, err)
	}

	var logged []string

	for _, l := range s.Output(0, 0).Lines {
		if l.Category == "logpoint" {
			logged = append(logged, strings.TrimSpace(l.Text))
		}
	}

	if strings.Join(logged, ",") != "lap 1,lap 2,lap 3,lap 4,lap 5" {
		t.Fatalf("logpoint lines = %q", logged)
	}
}

func TestManifestAttach(t *testing.T) {
	t.Parallel()

	attached := fakeProgram(t, 10)
	m := manager(t, userRegistry(t, fakeManifest(t, attached)))

	s := startFake(t, m, api.StartParams{Attach: &api.AttachSpec{PID: os.Getpid()}})
	if info := s.Info(); info.Mode != api.ModeAttach || info.State == api.StateExited {
		t.Fatalf("attached session = %+v", info)
	}

	if err := s.Detach(t.Context(), agent); err != nil {
		t.Fatalf("detach: %v", err)
	}
}

func TestManifestErrors(t *testing.T) {
	t.Parallel()

	m := manager(t, userRegistry(t, fakeManifest(t, "")))
	prog := fakeProgram(t, 3)

	_, err := m.Start(t.Context(), agent, api.StartParams{Lang: "fakelang", Attach: &api.AttachSpec{PID: os.Getpid()}})
	if api.CodeOf(err) != api.CodeUnsupported || !strings.Contains(err.Error(), "can't attach") {
		t.Fatalf("attach without a template = %v, want UNSUPPORTED_BY_ADAPTER", err)
	}

	_, err = m.Start(t.Context(), agent, api.StartParams{Lang: "fakelang", LaunchSpec: api.LaunchSpec{Program: prog, Options: map[string]string{"speed": "9"}}})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), `fakelang has no option "speed"`) {
		t.Fatalf("unknown option = %v, want INVALID_REQUEST", err)
	}

	_, err = m.Start(t.Context(), agent, api.StartParams{Lang: "fakelang", LaunchSpec: api.LaunchSpec{Program: prog, Options: map[string]string{"lines": "many"}}})
	if api.CodeOf(err) != api.CodeInvalidRequest {
		t.Fatalf("bad int option = %v, want INVALID_REQUEST", err)
	}
}
