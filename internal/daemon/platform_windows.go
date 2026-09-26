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
	"path/filepath"
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

// openShared opens path for reading in a way that lets replaceFile replace
// it while the returned file is still open: os.OpenInRoot uses
// FILE_SHARE_DELETE, unlike os.Open. Callers that read the token (every
// Dial) must use this, not os.Open/os.ReadFile, or replaceFile falls back to
// its retry loop for the whole time they hold it open.
func openShared(path string) (*os.File, error) {
	f, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		// OpenInRoot's error names only the base name.
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return f, nil
}

// replaceFile renames from to to, replacing to. from and to must be in the
// same directory (writeFileAtomic's precondition; enforced here because
// replaceFile needs one *os.Root on that directory).
//
// Windows refuses a plain rename over a file another process has open
// (ERROR_ACCESS_DENIED, even against a FILE_SHARE_DELETE handle;
// ERROR_SHARING_VIOLATION). os.Root.Rename instead asks for POSIX rename
// semantics (FILE_RENAME_POSIX_SEMANTICS): NTFS allows that even while a
// reader holds the file open, as long as the reader opened it sharing
// delete (openShared does; os.Open/os.ReadFile don't). Where POSIX rename
// isn't supported (FAT, older Windows), the stdlib falls back to a plain
// rename, so the retry loop below remains as a backstop for readers we
// don't control (an older eyedbg binary mid-upgrade, antivirus, indexers)
// and for that fallback path.
func replaceFile(from, to string) error {
	fromDir, toDir := filepath.Dir(from), filepath.Dir(to)
	if fromDir != toDir {
		return fmt.Errorf("replace %s with %s: not in the same directory", to, from)
	}

	root, err := os.OpenRoot(toDir)
	if err != nil {
		return err
	}
	defer root.Close()

	fromBase, toBase := filepath.Base(from), filepath.Base(to)

	deadline := time.Now().Add(replaceTimeout)
	pause := time.Millisecond

	for {
		err := root.Rename(fromBase, toBase)
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
