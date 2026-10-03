// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// streamingDriver is a fake driver that streams its build: PrepareWith
// writes lines to the options' Output before preparing as the fake does.
type streamingDriver struct {
	fakeDriver

	lines []string
}

func (d streamingDriver) PrepareWith(ctx context.Context, spec LaunchSpec, opts PrepareOptions) (Launch, error) {
	if opts.Output != nil {
		for _, l := range d.lines {
			if _, err := io.WriteString(opts.Output, l+"\n"); err != nil {
				return Launch{}, err
			}
		}
	}

	return d.Prepare(ctx, spec)
}

// newLaunchManager is newTestManagerWith with the adapters' stderr going to
// stderr.
func newLaunchManager(t *testing.T, drv Driver, stderr io.Writer) *Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{Drivers: []Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: stderr})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// TestLaunchConfigureHook checks that the hook runs with the session listed,
// starting and initialized, after the starter's own breakpoints, and that
// what it configures is in place before the program runs.
func TestLaunchConfigureHook(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	prog := writeProgram(t, 10)

	var seen []string

	hook := func(ctx context.Context, s *Session) error {
		select {
		case <-s.initialized:
			seen = append(seen, "initialized")
		default:
		}

		if st := s.Info().State; st == api.StateStarting {
			seen = append(seen, "starting")
		}

		if slices.ContainsFunc(m.List(), func(i api.SessionInfo) bool { return i.ID == s.ID }) {
			seen = append(seen, "listed")
		}

		s.mu.Lock()
		n := len(s.bps)
		s.mu.Unlock()

		if n == 1 {
			seen = append(seen, "starter's breakpoints")
		}

		bps, err := s.ReplaceBreakpoints(ctx, humanC, prog, []api.BreakpointSpec{{Line: 4}}, nil)
		if err != nil || len(bps) != 1 || bps[0].ID == 0 {
			t.Errorf("replace in the hook = %+v, %v", bps, err)
		}

		return nil
	}

	s, err := m.Launch(t.Context(), humanC, api.StartParams{
		Lang: "fake", LaunchSpec: api.LaunchSpec{Program: prog}, Breakpoints: []api.BreakpointSpec{{File: prog, Line: 7}},
	}, LaunchHooks{Configure: hook})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	want := []string{"initialized", "starting", "listed", "starter's breakpoints"}
	if !slices.Equal(seen, want) {
		t.Errorf("the hook saw %q, want %q", seen, want)
	}

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 4)
	expectStopped(t, resume(t, s, humanC, ExecContinue), reasonBreakpoint, 7)
}

// TestLaunchConfigureFails checks that a hook's error, or the context ending
// while the hook runs, fails the start: Launch returns that error, the
// session is forgotten and its adapter is gone.
func TestLaunchConfigureFails(t *testing.T) {
	t.Parallel()

	errHook := errors.New("the editor went away")

	tests := []struct {
		name string
		hook func(cancel context.CancelFunc) error
		want error
	}{
		{name: "hook error", hook: func(context.CancelFunc) error { return errHook }, want: errHook},
		{name: "canceled, hook returns nil", hook: func(cancel context.CancelFunc) error { cancel(); return nil }, want: context.Canceled},
		{name: "canceled, hook returns why", hook: func(cancel context.CancelFunc) error {
			cancel()

			return errHook
		}, want: errHook},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The adapter's stderr goes to this pipe, which ends once the
			// adapter (its only other writer) is gone.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			m := newLaunchManager(t, fakeDriver{}, w)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var id string

			_, err = m.Launch(ctx, humanC, api.StartParams{
				Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), Args: []string{"hang"}},
			}, LaunchHooks{Configure: func(_ context.Context, s *Session) error {
				id = s.ID

				return tt.hook(cancel)
			}})
			_ = w.Close()

			if !errors.Is(err, tt.want) {
				t.Fatalf("launch err = %v, want %v", err, tt.want)
			}

			if tt.want == errHook && err != errHook { //nolint:errorlint // Unchanged: the very error.
				t.Errorf("launch err = %#v, want the hook's error unchanged", err)
			}

			if got := m.List(); len(got) != 0 {
				t.Errorf("sessions after a failed launch = %+v, want none", got)
			}

			if _, err := m.Get(id); api.CodeOf(err) != api.CodeNoSession {
				t.Errorf("get %s after a failed launch: %v, want NO_SESSION", id, err)
			}

			drained := make(chan struct{})

			go func() {
				_, _ = io.Copy(io.Discard, r)

				close(drained)
			}()

			timer := time.NewTimer(testWait)
			defer timer.Stop()

			select {
			case <-drained:
			case <-timer.C:
				t.Fatal("the adapter still runs after the failed launch")
			}
		})
	}
}

// TestLaunchOutput checks that a streaming driver's build output reaches
// the Output hook in order, that a start without it streams nothing, and
// that a driver that can't stream still launches.
func TestLaunchOutput(t *testing.T) {
	t.Parallel()

	lines := []string{"Restoring…", "Building…", "Build succeeded."}

	tests := []struct {
		name   string
		drv    Driver
		output bool
		want   string
	}{
		{name: "streaming", drv: streamingDriver{lines: lines}, output: true, want: "Restoring…\nBuilding…\nBuild succeeded.\n"},
		{name: "streaming, no output hook", drv: streamingDriver{lines: lines}},
		{name: "not streaming", drv: fakeDriver{}, output: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, tt.drv)

			var out bytes.Buffer

			h := LaunchHooks{}
			if tt.output {
				h.Output = &out
			}

			s, err := m.Launch(t.Context(), humanC, api.StartParams{
				Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), StopOnEntry: true},
			}, h)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}

			expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)

			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}

			for _, e := range eventsOf(s, api.EventOutput) {
				if e.Text == "Building…\n" {
					t.Errorf("build output in the event log: %+v", e)
				}
			}
		})
	}
}

// TestLaunchRefuses checks that Launch only launches.
func TestLaunchRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    api.StartParams
	}{
		{name: "attach", p: api.StartParams{Attach: &api.AttachSpec{PID: os.Getpid()}}},
		{name: "test run", p: api.StartParams{Test: &api.TestSpec{Filter: "Adds"}}},
		{name: "unknown language", p: api.StartParams{Lang: "cobol"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManager(t, nil)

			if tt.p.Lang == "" {
				tt.p.Lang = "fake"
			}

			called := false

			_, err := m.Launch(t.Context(), humanC, tt.p, LaunchHooks{Configure: func(context.Context, *Session) error {
				called = true

				return nil
			}})
			expectCode(t, err, api.CodeInvalidRequest)

			if called || len(m.List()) != 0 {
				t.Errorf("hook called %v, sessions %+v; want neither", called, m.List())
			}
		})
	}
}

// terminalDriver is the fake driver prepared for a terminal: with
// PrepareOptions.Terminal its launch makes the fake ask for one.
type terminalDriver struct{ fakeDriver }

func (d terminalDriver) PrepareWith(ctx context.Context, spec LaunchSpec, opts PrepareOptions) (Launch, error) {
	l, err := d.Prepare(ctx, spec)
	if err == nil && opts.Terminal {
		l.Arguments["terminal"] = true
	}

	return l, err
}

// terminalCalls records a Terminal hook's calls and answers them.
type terminalCalls struct {
	mu    sync.Mutex
	calls []godap.RunInTerminalRequestArguments
	pid   int
	err   error
}

func (tc *terminalCalls) hook(_ context.Context, args godap.RunInTerminalRequestArguments) (int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.calls = append(tc.calls, args)

	return tc.pid, tc.err
}

// check checks the hook was called n times (0 or 1), with want.
func (tc *terminalCalls) check(t *testing.T, n int, want godap.RunInTerminalRequestArguments) {
	t.Helper()

	tc.mu.Lock()
	defer tc.mu.Unlock()

	if len(tc.calls) != n {
		t.Fatalf("hook called %d times, want %d", len(tc.calls), n)
	}

	if n == 0 {
		return
	}

	if got := tc.calls[0]; got.Title != want.Title || got.Cwd != want.Cwd || !slices.Equal(got.Args, want.Args) || got.Kind != want.Kind {
		t.Errorf("hook args = %+v, want %+v", got, want)
	}
}

func (tc *terminalCalls) count() int {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	return len(tc.calls)
}

// outputs are the session's output event texts.
func outputs(s *Session) []string {
	var out []string
	for _, e := range eventsOf(s, api.EventOutput) { //nolint:gocritic // A test helper: the copies don't matter.
		out = append(out, e.Text)
	}

	return out
}

// TestLaunchTerminal checks the terminal route: the adapter is told it may
// ask (and only then), its first runInTerminal in the start reaches the
// hook once and gets the hook's process id, and a second one in the start,
// one after Launch returned, and one of a start without the hook are
// refused.
func TestLaunchTerminal(t *testing.T) {
	t.Parallel()

	const refused = "fake: runInTerminal failed: runInTerminal is not supported by eyedbg\n"

	prog := fakeProgram(t)

	tests := []struct {
		name string
		drv  Driver
		// want: the output lines about runInTerminal during the start.
		want []string
	}{
		{name: "once", drv: terminalDriver{}, want: []string{"fake: runInTerminal processId=4242\n"}},
		{
			name: "a second in the start", drv: terminalDriver{fakeDriver{opts: daptest.Options{TerminalAgain: true}}},
			want: []string{"fake: runInTerminal processId=4242\n", refused},
		},
		// The adapter never asks during the start.
		{name: "none in the start", drv: streamingDriver{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, tt.drv)
			tc := &terminalCalls{pid: 4242}

			s, err := m.Launch(t.Context(), humanC, api.StartParams{
				Lang: "fake", LaunchSpec: api.LaunchSpec{Program: prog, StopOnEntry: true},
			}, LaunchHooks{Terminal: tc.hook})
			if err != nil {
				t.Fatalf("launch: %v", err)
			}

			expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)

			if got := outputs(s); !slices.Equal(got, tt.want) {
				t.Errorf("output = %q, want %q", got, tt.want)
			}

			calls := min(len(tt.want), 1)
			tc.check(t, calls, daptest.DefaultTerminal(prog))

			if got := evalValue(t, s, "$supportsRunInTerminalRequest"); got != "true" {
				t.Errorf("supportsRunInTerminalRequest = %s, want true", got)
			}

			// After Launch returned: refused at once, the hook not called.
			if got := evalValue(t, s, "$runInTerminal"); got != "failed" {
				t.Errorf("runInTerminal after the start = %s, want failed", got)
			}

			if got := outputs(s); len(got) != len(tt.want)+1 || got[len(got)-1] != refused {
				t.Errorf("output = %q, want the refusal last", got)
			}

			tc.check(t, calls, daptest.DefaultTerminal(prog))
		})
	}
}

// TestTerminalWithoutHook checks that a start without a Terminal hook
// neither tells the adapter it may run a terminal nor serves one: a CLI
// start whose adapter asks anyway fails with the refusal, and a driver
// that can't prepare for a terminal refuses a Launch with the hook.
func TestTerminalWithoutHook(t *testing.T) {
	t.Parallel()

	t.Run("capability", func(t *testing.T) {
		t.Parallel()

		m := newTestManagerWith(t, nil, terminalDriver{})

		s := start(t, m, humanC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
		if got := evalValue(t, s, "$supportsRunInTerminalRequest"); got != "false" {
			t.Errorf("CLI start: supportsRunInTerminalRequest = %s, want false", got)
		}

		l, err := m.Launch(t.Context(), humanC, api.StartParams{
			Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), StopOnEntry: true},
		}, LaunchHooks{})
		if err != nil {
			t.Fatal(err)
		}

		expectStopped(t, l.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)

		if got := evalValue(t, l, "$supportsRunInTerminalRequest"); got != "false" {
			t.Errorf("launch without a terminal: supportsRunInTerminalRequest = %s, want false", got)
		}
	})

	t.Run("CLI start asked anyway", func(t *testing.T) {
		t.Parallel()

		m := newTestManager(t, nil)

		_, err := m.Start(t.Context(), humanC, api.StartParams{
			Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), Args: []string{"terminal"}},
		})
		expectCode(t, err, api.CodeAdapterFailed)

		if !strings.Contains(err.Error(), "runInTerminal is not supported by eyedbg") {
			t.Errorf("err = %v, want the refusal", err)
		}
	})

	t.Run("driver can't", func(t *testing.T) {
		t.Parallel()

		m := newTestManager(t, nil)
		tc := &terminalCalls{pid: 1}

		_, err := m.Launch(t.Context(), humanC, api.StartParams{
			Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t)},
		}, LaunchHooks{Terminal: tc.hook})
		expectCode(t, err, api.CodeUnsupported)

		if tc.count() != 0 || len(m.List()) != 0 {
			t.Errorf("hook called %d times, sessions %+v; want neither", tc.count(), m.List())
		}
	})
}

// TestLaunchTerminalFails checks that a failing hook fails the start at
// once with its error (an *api.Error unchanged, anything else as
// ADAPTER_ERROR), the adapter answered with the failure, and nothing
// listed.
func TestLaunchTerminalFails(t *testing.T) {
	t.Parallel()

	apiErr := api.NewError(api.CodeAdapterFailed, "the editor didn't start the program's terminal: no", "h")

	tests := []struct {
		name     string
		pid      int
		err      error
		wantText string
	}{
		{name: "api error", err: apiErr, wantText: apiErr.Message},
		{name: "other error", err: errors.New("the editor went away"), wantText: "the editor went away"},
		{name: "no process id", pid: 0, wantText: "no process id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, terminalDriver{})
			tc := &terminalCalls{pid: tt.pid, err: tt.err}

			var s *Session

			hook := func(ctx context.Context, args godap.RunInTerminalRequestArguments) (int, error) {
				s, _ = m.Get(m.List()[0].ID)

				return tc.hook(ctx, args)
			}

			_, err := m.Launch(t.Context(), humanC, api.StartParams{
				Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), Args: []string{"hang"}},
			}, LaunchHooks{Terminal: hook})
			expectCode(t, err, api.CodeAdapterFailed)

			if tt.err == apiErr && err != apiErr { //nolint:errorlint // Unchanged: the very error.
				t.Errorf("err = %#v, want the hook's *api.Error unchanged", err)
			}

			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("err = %v, want %q", err, tt.wantText)
			}

			if len(m.List()) != 0 {
				t.Errorf("sessions after a failed launch = %+v, want none", m.List())
			}

			expectTerminalRefused(t, s)
		})
	}
}

// TestLaunchTerminalCanceled checks that the start's ctx ending while the
// hook runs ends the hook, fails the start and answers the adapter, and
// that Launch returns only once the hook has.
func TestLaunchTerminalCanceled(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, terminalDriver{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var s *Session

	returned := make(chan struct{})

	hook := func(hctx context.Context, _ godap.RunInTerminalRequestArguments) (int, error) {
		defer close(returned)

		s, _ = m.Get(m.List()[0].ID)

		cancel()
		<-hctx.Done()

		return 0, hctx.Err()
	}

	_, err := m.Launch(ctx, humanC, api.StartParams{
		Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), Args: []string{"hang"}},
	}, LaunchHooks{Terminal: hook})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("launch err = %v, want canceled", err)
	}

	select {
	case <-returned:
	default:
		t.Fatal("Launch returned before the hook did")
	}

	if len(m.List()) != 0 {
		t.Errorf("sessions after a canceled launch = %+v, want none", m.List())
	}

	expectTerminalRefused(t, s)
}

// expectTerminalRefused checks that the adapter of s, whose start failed
// with its terminal, reported nothing but the refusal it was answered
// with. A failed launch kills the adapter's process tree first thing
// (Manager.fail), so it may be gone before it reports even that. The kill
// is asynchronous: under load the adapter can still answer the teardown's
// disconnect before it dies, and it then notes that, which is no failure.
func expectTerminalRefused(t *testing.T, s *Session) {
	t.Helper()

	for _, got := range outputs(s) {
		if strings.HasPrefix(got, "fake: disconnect terminateDebuggee=") {
			continue
		}

		if !strings.HasPrefix(got, "fake: runInTerminal failed: eyedbg: ") {
			t.Errorf("adapter output %q, want only the failure it was answered with", got)
		}
	}
}
