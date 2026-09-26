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
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
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
