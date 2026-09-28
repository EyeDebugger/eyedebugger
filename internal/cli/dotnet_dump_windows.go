// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package cli

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// removeRetryTimeout bounds removeRetrying's retries: long enough for a
// small dump or trace's own runtime to notice its diagnostics connection
// dropped and close the file (well under a second, in testing), not so
// long that a canceled command hangs waiting on a large one it can't
// remove in time — that's left for the usual pruning instead.
const removeRetryTimeout = 3 * time.Second

// errSharingViolation is ERROR_SHARING_VIOLATION, which package syscall
// doesn't define (see internal/daemon/platform_windows.go's own copy: the
// same Windows behavior, a different package, not shared to avoid a
// dependency between them for one constant and one small retry loop).
const errSharingViolation syscall.Errno = 32

// removeRetrying removes path, retrying while the runtime that was writing
// it (a dump or a trace, canceled mid-write) still has it open: unlike
// Unix, Windows refuses to remove an open file (ERROR_SHARING_VIOLATION,
// sometimes surfaced as ERROR_ACCESS_DENIED instead). A file still open
// past removeRetryTimeout is left for the usual pruning.
func removeRetrying(path string) {
	deadline := time.Now().Add(removeRetryTimeout)
	pause := time.Millisecond

	for {
		err := os.Remove(path)
		if err == nil || !inUse(err) || time.Now().Add(pause).After(deadline) {
			return
		}

		time.Sleep(pause)

		pause = min(2*pause, 64*time.Millisecond)
	}
}

// inUse reports whether err is how Windows refuses to remove a file that's
// still open.
func inUse(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errSharingViolation)
}
