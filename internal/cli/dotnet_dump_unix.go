// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cli

import "os"

// removeRetrying removes path once: on Unix, unlinking a file another
// process still has open just detaches the name (its data outlives the
// unlink until every open handle to it closes), so a stray dump or trace
// canceled mid-write is gone from the directory at once. See the Windows
// version, which does need to retry.
func removeRetrying(path string) {
	_ = os.Remove(path)
}
