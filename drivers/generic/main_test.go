// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestMain lets the test binary double as the fake DAP adapter a test
// manifest names.
func TestMain(m *testing.M) {
	if daptest.MaybeRun() {
		return
	}

	m.Run()
}
