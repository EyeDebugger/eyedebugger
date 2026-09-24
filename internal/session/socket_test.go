// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// newSocketManager returns a manager of fake sessions on the connect
// transport (their socket paths go to drv.socket), stopped at cleanup.
func newSocketManager(t *testing.T, drv fakeDriver, stderr io.Writer, timeout time.Duration) *Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{
		Drivers: []Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: stderr, ConnectTimeout: timeout,
	})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// expectSocketGone checks that the adapter was given one socket, and that
// its directory is gone.
func expectSocketGone(t *testing.T, paths *socketPaths) {
	t.Helper()

	got := paths.all()
	if len(got) != 1 {
		t.Fatalf("socket paths = %q, want one", got)
	}

	if _, err := os.Stat(filepath.Dir(got[0])); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket directory %s: stat = %v, want it removed", filepath.Dir(got[0]), err)
	}
}

func TestSocketSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// end ends the session stopped at line 3.
		end  func(t *testing.T, m *Manager, s *Session)
		want string // end reason
	}{
		{"runs to its exit", func(t *testing.T, _ *Manager, s *Session) {
			t.Helper()

			if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
				t.Fatalf("after continue: %+v, want exited", snap.Session)
			}
		}, "the program terminated"},
		{"is stopped", func(t *testing.T, m *Manager, s *Session) {
			t.Helper()

			if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
				t.Fatal(err)
			}
		}, "stopped by agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			paths := &socketPaths{}
			m := newSocketManager(t, fakeDriver{socket: paths}, io.Discard, testWait)
			prog := fakeProgram(t)

			s := start(t, m, agentC, api.StartParams{
				LaunchSpec:  api.LaunchSpec{Program: prog, Args: []string{"lines=5"}},
				Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}},
			})

			// Gone as soon as the adapter connected.
			expectSocketGone(t, paths)

			expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), reasonBreakpoint, 3)

			if got := evalValue(t, s, "line"); got != "3" {
				t.Errorf("eval line = %q, want 3", got)
			}

			tt.end(t, m, s)

			info := s.Info()
			if info.State != api.StateExited || info.EndReason != tt.want {
				t.Errorf("after the end: %+v, want exited (%s)", info, tt.want)
			}
		})
	}
}

// TestSocketAdapterFails checks that an adapter that doesn't connect fails
// the start and is killed, and that its socket is gone.
func TestSocketAdapterFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    daptest.Options
		timeout time.Duration
		want    string // in the error message
	}{
		{"exits before connecting", daptest.Options{ExitBeforeConnect: true}, testWait, "the debug adapter exited before connecting"},
		{"never connects", daptest.Options{HangBeforeConnect: true}, 200 * time.Millisecond, "the debug adapter did not connect within 200ms"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The adapter's output goes to this pipe, which ends once the
			// adapter (its only other writer) is gone.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			paths := &socketPaths{}
			m := newSocketManager(t, fakeDriver{opts: tt.opts, socket: paths}, w, tt.timeout)

			_, err = m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t)}})
			_ = w.Close()

			expectCode(t, err, api.CodeAdapterFailed)

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}

			expectSocketGone(t, paths)

			if got := m.List(); len(got) != 0 {
				t.Errorf("sessions after a failed start = %+v, want none", got)
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
				t.Fatal("the adapter still runs after the failed start")
			}
		})
	}
}

// TestSocketPathTooLong checks the error for a temporary directory too
// long for a socket path. Not parallel: it sets the environment.
func TestSocketPathTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}

	prog := fakeProgram(t)

	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, long)
	}

	paths := &socketPaths{}
	m := newSocketManager(t, fakeDriver{socket: paths}, io.Discard, testWait)

	_, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: prog}})
	expectCode(t, err, api.CodeAdapterFailed)

	e, _ := errors.AsType[*api.Error](err)
	if !strings.Contains(e.Message, "over the limit of 103") || e.Hint != "set TMPDIR (TEMP on Windows) to a shorter directory" {
		t.Errorf("err = %q (hint %q), want the socket path limit and how to fix it", e.Message, e.Hint)
	}

	if got := paths.all(); len(got) != 0 {
		t.Errorf("adapter started with sockets %q, want not started", got)
	}

	if left, err := os.ReadDir(long); err != nil || len(left) != 0 {
		t.Errorf("left in %s: %v, %v; want nothing", long, left, err)
	}
}
