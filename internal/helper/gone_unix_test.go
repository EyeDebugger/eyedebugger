// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package helper

import (
	"context"
	"errors"
	"fmt"

	"github.com/eyedebugger/eyedebugger/internal/proc"
)

// processGone returns nil if no process has pid. Close has reaped the
// helper (cmd.Wait), so its pid names no process, not even a zombie.
func processGone(pid int) error {
	_, err := proc.Lookup(context.Background(), pid)

	switch {
	case errors.Is(err, proc.ErrNoProcess):
		return nil
	case err != nil:
		return fmt.Errorf("lookup: %w", err)
	default:
		return errors.New("lookup found it")
	}
}
