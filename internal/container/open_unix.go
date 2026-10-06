// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package container

import (
	"os"
	"syscall"
)

// openNoBlock opens p for reading without blocking: a FIFO swapped in after a
// regular-file check fails the check on the opened file instead of hanging.
func openNoBlock(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:wrapcheck // The caller checks what it opened; its error is the caller's to wrap.
}
