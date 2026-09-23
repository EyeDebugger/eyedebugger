// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoProcess means no process has the pid.
var ErrNoProcess = errors.New("no such process")

// Info describes a process.
type Info struct {
	PID int
	// Name is the executable's name, without its directory; it may be
	// empty when the system doesn't tell.
	Name string
	// Owner names the user the process runs as (a user name, or a uid or
	// SID when it can't be resolved); empty when the system doesn't tell.
	Owner string
	// SameUser means the process runs as the calling user.
	SameUser bool
}

// Lookup returns what the system tells about process pid, or ErrNoProcess.
func Lookup(ctx context.Context, pid int) (Info, error) {
	if pid < 1 {
		return Info{}, fmt.Errorf("pid %d: %w", pid, ErrNoProcess)
	}

	return lookup(ctx, pid)
}
