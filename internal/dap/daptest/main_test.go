// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest_test

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestMain lets the test binary double as the fake test runner.
func TestMain(m *testing.M) {
	daptest.MaybeRunRunner()
	m.Run()
}
