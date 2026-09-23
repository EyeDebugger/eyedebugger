// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestMain lets the test binary double as the fake DAP adapter.
func TestMain(m *testing.M) {
	if daptest.MaybeRun() {
		return
	}

	m.Run()
}

// fakeDriver runs programs of 10 lines on the fake adapter; "lines=2" makes
// them 2 lines long.
type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }

func (fakeDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	lines := 10
	if slices.Contains(spec.Args, "lines=2") {
		lines = 2
	}

	pa := daptest.ProgramArgs{Program: spec.Program, Lines: lines, StopAtEntry: spec.StopOnEntry}
	if slices.Contains(spec.Args, "laps=3") {
		pa.Laps = 3
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: pa.Map(),
	}, nil
}

// PrepareAttach attaches to a hanging fake program of 10 lines, prog.txt
// in the temp dir.
func (fakeDriver) PrepareAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake",
		Arguments: daptest.ProgramArgs{Program: attachProgram(), Lines: 10, Hang: true, ProcessID: spec.PID}.Map(),
	}, nil
}

// attachProgram is the program PrepareAttach attaches to.
func attachProgram() string { return filepath.Join(os.TempDir(), "eyedbg-daemon-test-attached.txt") }
