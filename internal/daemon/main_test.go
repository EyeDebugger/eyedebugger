// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"slices"
	"testing"

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

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: daptest.Arguments(spec.Program, lines, spec.StopOnEntry, false),
	}, nil
}
