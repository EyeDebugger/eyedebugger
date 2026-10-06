// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import "os"

// openNoBlock opens p for reading (Windows has no FIFO a path can name).
func openNoBlock(p string) (*os.File, error) {
	return os.Open(p) //nolint:wrapcheck // The caller checks what it opened; its error is the caller's to wrap.
}
