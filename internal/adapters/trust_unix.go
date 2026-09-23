// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package adapters

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkTrust checks that p (its target, for a symlink) is the user's own
// and that no one else can write it (see checkInfo).
func checkTrust(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	return checkInfo(p, info)
}

// checkInfo checks the file or directory p, described by info, as
// OpenSSH's StrictModes does: owned by the user and writable by neither
// group nor others, whatever the group (a user's primary group need not
// be private: on macOS it is the shared "staff"). User manifests and
// project venvs name commands eyedbg runs.
func checkInfo(p string, info fs.FileInfo) error {
	return trustChecker{uid: os.Getuid()}.check(p, info)
}

// ownedByUser reports whether the user owns the file or directory p.
func ownedByUser(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}

	st, ok := info.Sys().(*syscall.Stat_t)

	return ok && int(st.Uid) == os.Getuid()
}

// trustChecker is checkInfo with the user injectable.
type trustChecker struct {
	uid int
}

func (c trustChecker) check(p string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("check permissions of %s: no owner information", p)
	}

	mode := info.Mode().Perm()

	switch {
	case int(st.Uid) != c.uid:
		return fmt.Errorf("%s is owned by uid %d, not you (uid %d); it must be your own", p, st.Uid, c.uid)
	case mode&0o022 != 0:
		return fmt.Errorf("%s is writable by others (%s); run 'chmod go-w %s'", p, mode, p)
	}

	return nil
}

// openManifest opens a user manifest for reading without blocking: a FIFO
// swapped in after the regular-file check fails the check on the opened
// file instead of hanging the loader.
func openManifest(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
