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

// detachedProcess is DETACHED_PROCESS: no console is inherited or created.
const detachedProcess = 0x00000008

// detach makes cmd outlive the caller and its console.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
}
