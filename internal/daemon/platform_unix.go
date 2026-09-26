// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

// checkPrivate refuses a runtime directory owned by another user and tightens
// one of ours that others can access.
func checkPrivate(dir string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("runtime directory %s is owned by uid %d, not by you (uid %d)", dir, st.Uid, os.Getuid())
	}

	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A directory needs the x bit; 0700 is owner-only.
			return fmt.Errorf("restrict runtime directory permissions: %w", err)
		}
	}

	return nil
}

// replaceFile renames from to to, replacing to even while it is open.
func replaceFile(from, to string) error { return os.Rename(from, to) }

// openShared opens path for reading. Unix rename already replaces an open
// file without disturbing readers, so there is nothing special to ask for.
func openShared(path string) (*os.File, error) { return os.Open(path) }

// lockFile takes an exclusive, non-blocking lock on path. The OS releases it
// when the process exits, however it exits.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return f, nil
}

// detach makes cmd outlive the caller and its terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
