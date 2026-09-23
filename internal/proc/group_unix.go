// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// StartGroup starts cmd as the leader of a new process group, so that
// KillGroup reaches every process it starts.
func StartGroup(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	return nil
}

// KillGroup kills every process of the group StartGroup started cmd in. A
// group that is already gone is not an error.
func KillGroup(_ context.Context, cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", cmd.Process.Pid, err)
	}

	return nil
}
