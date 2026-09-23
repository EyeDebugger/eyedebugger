// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// lookup reads /proc/<pid>: its owner is the owner of the directory.
func lookup(_ context.Context, pid int) (Info, error) {
	dir := "/proc/" + strconv.Itoa(pid)

	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, fmt.Errorf("pid %d: %w", pid, ErrNoProcess)
	}

	if err != nil {
		return Info{}, fmt.Errorf("look up pid %d: %w", pid, err)
	}

	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, fmt.Errorf("look up pid %d: no owner in %s", pid, dir)
	}

	info := Info{PID: pid, Owner: ownerName(sys.Uid), SameUser: int(sys.Uid) == os.Getuid()}

	if comm, err := os.ReadFile(dir + "/comm"); err == nil {
		info.Name = strings.TrimSpace(string(comm))
	}

	return info, nil
}

// ownerName returns uid's user name, or "uid N".
func ownerName(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}

	return "uid " + id
}
