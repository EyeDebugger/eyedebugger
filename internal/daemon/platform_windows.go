// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// checkPrivate is a no-op on Windows: the default runtime directory lives in
// %LocalAppData%, whose inherited ACL already limits it to the user.
func checkPrivate(string, fs.FileInfo) error { return nil }

// lockFile opens path with no sharing, which Windows enforces until the
// handle closes (including on process exit).
func lockFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrAlreadyRunning
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return os.NewFile(uintptr(h), path), nil
}

// errSharingViolation is ERROR_SHARING_VIOLATION, which package syscall
// doesn't define.
const errSharingViolation syscall.Errno = 32

// replaceTimeout bounds replaceFile's retries: Go's own cmd/internal/robustio
// retries these errors for as long, for the same reason.
const replaceTimeout = 2 * time.Second

// replaceFile renames from to to, replacing to. Windows refuses to replace
// a file another process has open (ERROR_ACCESS_DENIED, even when it was
// opened with FILE_SHARE_DELETE; ERROR_SHARING_VIOLATION), and clients
// open the token on every Dial: a daemon started while one polls for it,
// after the last daemon was killed and left its token behind, would fail
// to start. Readers hold the file only for as long as a read takes, so the
// rename is retried, with a growing pause, for up to replaceTimeout.
func replaceFile(from, to string) error {
	deadline := time.Now().Add(replaceTimeout)
	pause := time.Millisecond

	for {
		err := os.Rename(from, to)
		if !inUse(err) || time.Now().Add(pause).After(deadline) {
			return err
		}

		time.Sleep(pause)

		pause = min(2*pause, 64*time.Millisecond)
	}
}

// inUse reports whether err is how Windows refuses to replace a file that
// is open.
func inUse(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errSharingViolation)
}

// detachedProcess is DETACHED_PROCESS: no console is inherited or created.
const detachedProcess = 0x00000008

// detach makes cmd outlive the caller and its console.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
}
