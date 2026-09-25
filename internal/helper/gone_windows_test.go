// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package helper

import (
	"errors"
	"fmt"
	"syscall"
)

const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION.
	processQueryLimitedInformation = 0x1000
	// errInvalidParameter is what OpenProcess fails with for a pid no
	// process has.
	errInvalidParameter syscall.Errno = 87
)

// processGone returns nil if pid names no running process. A pid that
// opens is not enough to say it runs: an exited process's object — and
// with it its pid — outlives the process until the system drops its last
// reference, which in tests took up to 2 s after Close had reaped the
// helper (WaitForSingleObject), with no user-mode handle to it left. So
// an openable pid is checked for having exited.
func processGone(pid int) error {
	h, err := syscall.OpenProcess(processQueryLimitedInformation|syscall.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // A helper's pid, > 0.

	switch {
	case errors.Is(err, errInvalidParameter):
		return nil
	case err != nil:
		return fmt.Errorf("open: %w", err)
	}
	defer syscall.CloseHandle(h) //nolint:errcheck // Closing a query handle; nothing to do on failure.

	event, err := syscall.WaitForSingleObject(h, 0)

	switch event {
	case syscall.WAIT_OBJECT_0:
		return nil
	case syscall.WAIT_TIMEOUT:
		return errors.New("still running")
	default:
		return fmt.Errorf("wait: %w", err)
	}
}
