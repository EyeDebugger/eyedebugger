// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestLookupExitedButOpen checks a process that has exited is gone even
// while a handle keeps it openable.
func TestLookupExitedButOpen(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), exe)
	cmd.Env = append(os.Environ(), envHelper+"=exit")

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	pid := cmd.Process.Pid

	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)) //nolint:gosec // A child's pid.
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h) //nolint:errcheck // Test cleanup.

	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	if _, err := Lookup(t.Context(), pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Lookup(exited pid %d) err = %v, want ErrNoProcess", pid, err)
	}
}
