// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION.
	processQueryLimitedInformation = 0x1000
	// errInvalidParameter is what OpenProcess fails with for a pid no
	// process has.
	errInvalidParameter syscall.Errno = 87
)

// lookup compares the process token's user with ours. A process we may not
// even query belongs to another user.
func lookup(_ context.Context, pid int) (Info, error) {
	info := Info{PID: pid, Name: exeName(uint32(pid))} //nolint:gosec // pid >= 1, checked by Lookup.

	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)) //nolint:gosec // pid >= 1, checked by Lookup.

	switch {
	case errors.Is(err, errInvalidParameter):
		return Info{}, fmt.Errorf("pid %d: %w", pid, ErrNoProcess)
	case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
		return info, nil
	case err != nil:
		return Info{}, fmt.Errorf("look up pid %d: %w", pid, err)
	}
	defer syscall.CloseHandle(h) //nolint:errcheck // Closing a query handle; nothing to do on failure.

	var token syscall.Token
	if err := syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &token); err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return info, nil
		}

		return Info{}, fmt.Errorf("look up pid %d: %w", pid, err)
	}
	defer token.Close()

	theirs, err := tokenSID(token)
	if err != nil {
		return Info{}, fmt.Errorf("look up pid %d: %w", pid, err)
	}

	self, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return Info{}, fmt.Errorf("open our token: %w", err)
	}
	defer self.Close()

	ours, err := tokenSID(self)
	if err != nil {
		return Info{}, fmt.Errorf("read our token: %w", err)
	}

	info.Owner, info.SameUser = theirs, theirs == ours

	return info, nil
}

// tokenSID returns the string SID of token's user.
func tokenSID(token syscall.Token) (string, error) {
	u, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("token user: %w", err)
	}

	sid, err := u.User.Sid.String()
	if err != nil {
		return "", fmt.Errorf("token SID: %w", err)
	}

	return sid, nil
}

// exeName finds pid's executable name in a process snapshot ("" if absent).
func exeName(pid uint32) string {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(snap) //nolint:errcheck // Closing a snapshot handle; nothing to do on failure.

	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))

	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if e.ProcessID == pid {
			return syscall.UTF16ToString(e.ExeFile[:])
		}
	}

	return ""
}
