// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// lookup asks ps, which exits 1 (printing nothing) for no such process.
func lookup(ctx context.Context, pid int) (Info, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "uid=,comm=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // A fixed program; the only variable argument is a number.

	line := strings.TrimSpace(string(out))
	if line == "" {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err == nil || ok && exitErr.ExitCode() == 1 {
			return Info{}, fmt.Errorf("pid %d: %w", pid, ErrNoProcess)
		}

		return Info{}, fmt.Errorf("look up pid %d: %w", pid, err)
	}

	uidText, comm, _ := strings.Cut(line, " ")

	uid, err := strconv.Atoi(uidText)
	if err != nil {
		return Info{}, fmt.Errorf("look up pid %d: unexpected ps output %q", pid, line)
	}

	info := Info{PID: pid, Name: filepath.Base(strings.TrimSpace(comm)), Owner: "uid " + uidText, SameUser: uid == os.Getuid()}
	if u, err := user.LookupId(uidText); err == nil {
		info.Owner = u.Username
	}

	return info, nil
}
