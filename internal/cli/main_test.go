// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
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

// fakeDriver runs 10-line programs on the fake adapter.
type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }

func (fakeDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: daptest.Arguments(spec.Program, 10, spec.StopOnEntry, false),
	}, nil
}
