// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"os"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

// TestMain lets the test binary double as the fake docker of the container
// tests.
func TestMain(m *testing.M) {
	containertest.MaybeRun()

	os.Exit(m.Run())
}
