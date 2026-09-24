// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestMain lets the test binary double as the fakelang manifest's DAP
// adapter (EYEDBG_TEST_FAKE_ADAPTER=1; see langs_test.go), and otherwise
// removes the eyedbg/eyedbgd binaries binariesFor built, once every test
// in the package has run.
func TestMain(m *testing.M) {
	if daptest.MaybeRun() {
		return
	}

	m.Run()
	removeBinaries()
}
