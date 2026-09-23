// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestMain lets the test binary double as the fake DAP adapter and the fake
// test runner.
func TestMain(m *testing.M) {
	daptest.MaybeRunRunner()

	if daptest.MaybeRun() {
		return
	}

	m.Run()
}
