// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// StartGroup starts cmd in a new process group, so that KillGroup can kill
// its process tree.
func StartGroup(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	return nil
}

// KillGroup kills cmd and every process it started (taskkill /T), then cmd
// itself in case taskkill could not.
func KillGroup(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	// Both fail once the tree is gone (taskkill: not found; Kill: access
	// denied or done), which is the goal: neither error tells more.
	_ = exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() //nolint:gosec // A fixed program; the only variable argument is a number.
	_ = cmd.Process.Kill()

	return nil
}
