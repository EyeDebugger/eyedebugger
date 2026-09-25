// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package artifacts

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkPrivate refuses a directory another user owns and removes group and
// other access from one of the user's (the daemon's runtime directory rule).
func checkPrivate(dir string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	if err := checkOwner(dir, int(st.Uid), os.Getuid()); err != nil {
		return err
	}

	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A directory needs the x bit; 0700 is owner-only.
			return fmt.Errorf("restrict %s to you: %w", dir, err)
		}
	}

	return nil
}

// checkOwner refuses dir unless uid (its owner) is me.
func checkOwner(dir string, uid, me int) error {
	if uid != me {
		return fmt.Errorf("%s is owned by uid %d, not by you (uid %d); eyedbg keeps dumps only in a directory you own", dir, uid, me)
	}

	return nil
}
