// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
)

// EnvCancelOnStdinEOF is the opt-in env var (main.go) that makes eyedbg
// treat stdin EOF, or a read error on it, like Ctrl-C: it cancels the root
// context the same way os.Interrupt does. Off by default — an agent's
// shell commonly gives stdin at EOF (/dev/null), which would cancel every
// command at once — a tool that can't send Ctrl-C (the VS Code extension,
// on Windows) sets it and closes stdin to cancel instead.
const EnvCancelOnStdinEOF = "EYEDBG_CANCEL_ON_STDIN_EOF"

// CancelOnEOF returns a context derived from parent that is canceled the
// same way parent's own cancellation is (context.Canceled) once r's Read
// returns EOF or any other error. It takes a reader rather than os.Stdin
// itself so it can be tested with an io.Pipe, not a global.
//
// The draining goroutine blocks on r.Read until that happens or parent
// ends first; either way it exits on its own and never keeps the process
// alive or delays its exit (a process exit closes the reader out from
// under a still-blocked Read on a real stdin pipe).
func CancelOnEOF(parent context.Context, r io.Reader) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	go func() {
		buf := make([]byte, 512)

		for {
			if _, err := r.Read(buf); err != nil {
				cancel()

				return
			}

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	return ctx, cancel
}
