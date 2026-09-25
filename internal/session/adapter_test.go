// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// pauseHint is the fake "b" adapter's hint for a refused pause.
const pauseHint = "set a breakpoint instead"

// selectingDriver is a fake driver with two adapters: "a", its default,
// and "b", which can't pause and sets by evaluate (SharpDbg's shape).
type selectingDriver struct {
	fakeDriver
}

func (d selectingDriver) WithAdapter(name string) (Driver, error) {
	switch name {
	case "a":
		d.knobs = Launch{AdapterName: "a"}
	case "b":
		d.knobs = Launch{AdapterName: "b", PauseUnsupported: pauseHint, SetByEval: true}
	default:
		return nil, api.NewError(api.CodeInvalidRequest, "fake has no adapter "+name, "use a or b")
	}

	return d.fakeDriver, nil
}

func TestStartWithAdapter(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)
	base := fakeDriver{
		knobs:  Launch{AdapterName: "a"},
		attach: daptest.ProgramArgs{Program: prog, Lines: 5, Hang: true},
		runner: daptest.RunnerHang,
	}

	tests := []struct {
		name    string
		drv     Driver
		adapter string
		mode    string // "", api.ModeAttach or api.ModeTest
		want    string // SessionInfo.Adapter; "" when the start fails
		code    api.Code
	}{
		{name: "default", drv: selectingDriver{base}, want: "a"},
		{name: "chosen", drv: selectingDriver{base}, adapter: "b", want: "b"},
		{name: "chosen, attach", drv: selectingDriver{base}, adapter: "b", mode: api.ModeAttach, want: "b"},
		{name: "chosen, test run", drv: selectingDriver{base}, adapter: "b", mode: api.ModeTest, want: "b"},
		{name: "unknown", drv: selectingDriver{base}, adapter: "c", code: api.CodeInvalidRequest},
		{name: "unknown, test run", drv: selectingDriver{base}, adapter: "c", mode: api.ModeTest, code: api.CodeInvalidRequest},
		{name: "no selector", drv: base, adapter: "a", code: api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, tt.drv)
			p := api.StartParams{Lang: "fake", Adapter: tt.adapter}

			switch tt.mode {
			case api.ModeAttach:
				p.Attach = &api.AttachSpec{PID: os.Getpid()}
			case api.ModeTest:
				p.Test = &api.TestSpec{Filter: "Adds"}
			default:
				p.Program, p.StopOnEntry = prog, true
			}

			s, err := m.Start(t.Context(), agentC, p)
			if tt.code != "" {
				expectCode(t, err, tt.code)

				if n := len(m.List()); n != 0 {
					t.Errorf("a refused start left %d sessions", n)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got := s.Info().Adapter; got != tt.want {
				t.Errorf("SessionInfo.Adapter = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPauseUnsupported: a driver's PauseUnsupported refuses pause before
// the lease and the exec event, and leaves the session running and
// stoppable.
func TestPauseUnsupported(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, fakeDriver{knobs: Launch{AdapterName: "b", PauseUnsupported: pauseHint}})
	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	if st := s.Info().State; st != api.StateRunning {
		t.Fatalf("state = %s, want running", st)
	}

	execs := len(eventsOf(s, api.EventExec))

	// The human would take the free lease if the pause were admitted.
	_, err := s.Exec(t.Context(), humanC, ExecPause, 0)
	expectCode(t, err, api.CodeUnsupported)

	if e, ok := err.(*api.Error); !ok || e.Hint != pauseHint || e.Message == "" { //nolint:errorlint // Returned unwrapped.
		t.Errorf("pause err = %#v, want the driver's hint %q", err, pauseHint)
	}

	info := s.Info()
	if info.State != api.StateRunning || info.Lease == nil || info.Lease.Holder != agentC.ID {
		t.Errorf("after the refused pause: %+v (lease %+v), want running with the agent's lease", info, info.Lease)
	}

	if n := len(eventsOf(s, api.EventExec)); n != execs {
		t.Errorf("the refused pause logged %d exec events", n-execs)
	}

	if err := s.Terminate(t.Context(), agentC); err != nil {
		t.Fatalf("stop after the refused pause: %v", err)
	}
}
