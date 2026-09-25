// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/helper/helpertest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestMain lets the test binary double as the fake DAP adapter and the
// fake .NET side helper.
func TestMain(m *testing.M) {
	helpertest.MaybeRun()

	if daptest.MaybeRun() {
		return
	}

	m.Run()
}

// fakeDriver runs 10-line programs on the fake adapter; expressions with
// "(" count as method calls. It attaches to a hanging 10-line program,
// attached.txt in the temp dir.
type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }

func (fakeDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: daptest.Arguments(spec.Program, 10, spec.StopOnEntry, false), SideEffects: calls,
	}, nil
}

func (fakeDriver) PrepareAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", SideEffects: calls,
		Arguments: daptest.ProgramArgs{Program: attachedProgram(), Lines: 10, Hang: true, ProcessID: spec.PID}.Map(),
	}, nil
}

// attachedProgram is the program the fake driver attaches to.
func attachedProgram() string { return filepath.Join(os.TempDir(), "attached.txt") }

func calls(expr string) (string, bool) {
	if strings.Contains(expr, "(") {
		return "call a method", true
	}

	return "", false
}
