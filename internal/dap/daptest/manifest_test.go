// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest_test

import (
	"encoding/json"
	"runtime"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// testBinaryPath is an absolute path per filepath.IsAbs on the current OS:
// a Unix-style path is not absolute on Windows (no volume name).
var testBinaryPath = map[bool]string{true: `C:\path\to\test-binary`, false: "/path/to/test-binary"}[runtime.GOOS == "windows"]

func TestUserManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attached   string
		wantAttach bool
	}{
		{name: "no attach", attached: "", wantAttach: false},
		{name: "attach", attached: "/src/prog.txt", wantAttach: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checkUserManifest(t, tt.attached, tt.wantAttach)
		})
	}
}

// checkUserManifest marshals and parses [daptest.UserManifest] for attached,
// and checks the result against the fixed fields it always sets.
func checkUserManifest(t *testing.T, attached string, wantAttach bool) {
	t.Helper()

	raw, err := json.Marshal(daptest.UserManifest(testBinaryPath, attached))
	if err != nil {
		t.Fatal(err)
	}

	m, err := adapters.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, raw)
	}

	if m.Name != "fakedbg" {
		t.Errorf("Name = %q, want fakedbg", m.Name)
	}

	if m.LanguageName() != "fakelang" {
		t.Errorf("LanguageName() = %q, want fakelang", m.LanguageName())
	}

	if m.Adapter.Entry != testBinaryPath {
		t.Errorf("Adapter.Entry = %q, want %q", m.Adapter.Entry, testBinaryPath)
	}

	if m.Adapter.Environment[daptest.EnvFakeAdapter] != "1" {
		t.Errorf("Adapter.Environment[%s] = %q, want 1", daptest.EnvFakeAdapter, m.Adapter.Environment[daptest.EnvFakeAdapter])
	}

	if got := m.Attach != nil; got != wantAttach {
		t.Errorf("Attach != nil = %v, want %v", got, wantAttach)
	}
}
