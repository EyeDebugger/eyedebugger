// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// childListener listens on loopback for a fake child, closed at cleanup.
func childListener(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// acceptChild accepts the fake child dialing ln, bounding every later read
// by testWait; closing its connection at cleanup ends a child still alive.
func acceptChild(t *testing.T, ln net.Listener) *daptest.Child {
	t.Helper()

	// Accept has no deadline of its own: a child that never dials fails
	// the test by the listener closing.
	timer := time.AfterFunc(testWait, func() { _ = ln.Close() })
	defer timer.Stop()

	c, err := daptest.AcceptChild(ln)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Conn.Close() })

	if err := c.Conn.SetDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}

	return c
}

// failingLaunch returns a manager whose fake adapter, with opts, starts a
// child dialing child, and the params to start prog with: on a socket, or
// as a launched test run.
func failingLaunch(t *testing.T, prog, child string, opts daptest.Options, socket, testRun bool) (*Manager, api.StartParams) {
	t.Helper()

	drv := fakeDriver{opts: opts}
	drv.opts.Child = child

	if socket {
		drv.socket = &socketPaths{}
	}

	p := api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: prog, Args: []string{"hang"}}}

	if testRun {
		drv.launch = &daptest.ProgramArgs{Program: prog, Lines: 5, Hang: true}
		p = api.StartParams{Lang: "fake", Test: &api.TestSpec{Filter: "Adds"}}
	}

	return newTestManagerWith(t, nil, drv), p
}

// TestFailedLaunchKillsProgram: a launch start that fails once the adapter
// runs kills the adapter's process group or tree, and with it the program
// the adapter launched (a fake child it leaves running), before the
// session is forgotten.
func TestFailedLaunchKillsProgram(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    daptest.Options
		socket  bool
		testRun bool
		want    string // in the start's error
	}{
		{name: "configurationDone fails", opts: daptest.Options{FailConfigurationDone: true}, want: "0x80004005"},
		{name: "configurationDone fails on a socket", opts: daptest.Options{FailConfigurationDone: true}, socket: true, want: "0x80004005"},
		{name: "launch fails", opts: daptest.Options{FailLaunch: true}, want: "Failed to launch"},
		{name: "launched test run fails", opts: daptest.Options{FailConfigurationDone: true}, testRun: true, want: "0x80004005"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prog := launchedProgram(t, 5)
			ln := childListener(t)
			m, p := failingLaunch(t, prog, ln.Addr().String(), tt.opts, tt.socket, tt.testRun)

			started := make(chan error, 1)

			go func() {
				_, err := m.Start(t.Context(), agentC, p)
				started <- err
			}()

			// The adapter goes on only once the child is accepted.
			child := acceptChild(t, ln)

			err := <-started
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("start = %v, want the adapter's error with %q", err, tt.want)
			}

			if err := child.Gone(); err != nil {
				t.Fatalf("the launched program (pid %d) outlived the failed start: %v", child.PID, err)
			}

			if n := len(m.List()); n != 0 {
				t.Errorf("%d sessions left, want the failed one forgotten", n)
			}
		})
	}
}

// TestFailedAttachKeepsProcesses: an attach start that fails kills neither
// its target nor anything the adapter started.
func TestFailedAttachKeepsProcesses(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)

	targetLn := childListener(t)

	cmd, err := daptest.ChildCommand(targetLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Wait() }) // after its connection closes (cleanups run last first)

	target := acceptChild(t, targetLn)

	adapterLn := childListener(t)
	drv := fakeDriver{
		opts:   daptest.Options{Child: adapterLn.Addr().String()},
		attach: daptest.ProgramArgs{Program: prog, Lines: 5, Hang: true, FailAttach: true},
	}
	m := newTestManagerWith(t, nil, drv)

	started := make(chan error, 1)

	go func() {
		_, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", Attach: &api.AttachSpec{PID: cmd.Process.Pid}})
		started <- err
	}()

	adapterChild := acceptChild(t, adapterLn)

	expectCode(t, <-started, api.CodeAttachFailed)

	for name, c := range map[string]*daptest.Child{"the attach target": target, "the adapter's child": adapterChild} {
		if err := c.Alive(); err != nil {
			t.Errorf("%s (pid %d) after the failed attach: %v, want it alive", name, c.PID, err)
		}
	}

	if cmd.Process.Pid != target.PID {
		t.Errorf("target pid %d, child says %d", cmd.Process.Pid, target.PID)
	}
}
